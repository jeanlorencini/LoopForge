#!/usr/bin/env python3
"""Derive the reliability update from a supported SOP workflow export."""

import argparse
from hashlib import sha256
import json
from pathlib import Path
import runpy
import tempfile
from textwrap import dedent, indent


LEGACY = runpy.run_path(str(Path(__file__).with_name("apply-sop-runtime-preflight.py")))
CHAIN = (
    "git-branch", "spec", "validate-spec", "tests", "validate-tests", "approve-tests",
    "reserve", "plan", "implement", "verify", "review", "fix", "verify-final",
    "git-commit", "approve-merge", "git-merge",
)
SOURCE_SCRIPTS = {
    "git-branch": "f138c7e87035ea7c71f95ccda7d1d26bbd241134fe3f02b7875597677fa89ad6",
    "validate-spec": "b89ac6550837bd70e5e936430659deb37da4b10b241cf3ee4e8ed3d715923d8c",
    "validate-tests": LEGACY["PREFLIGHT_VALIDATOR"],
    "reserve": "26d61d8a8fb51313e03c2a533562f463ab4c8200f7495770861cd89a107f7c91",
    "verify": "2a1e7de38431c7c14adac868d71fb8210b9ff302c0a7b9c6571ef7e2b48c6439",
    "verify-final": "2a1e7de38431c7c14adac868d71fb8210b9ff302c0a7b9c6571ef7e2b48c6439",
    "git-commit": "7642a2e709bcdf101a77186634ea44891d449ed06d0c9405f6067b4546522b92",
    "git-merge": "cc171d52f5fccb8764776f981dc1a469cfb5d419f4bf674af998c53f38b6627a",
}

# The transformed scripts preserve parts of their source without storing the export.
PREVIOUS_SCRIPTS = {
    "git-branch": SOURCE_SCRIPTS["git-branch"],
    "validate-spec": "8d16833106b454a0f68c12ca64e18316fca1e1a21c95b4505a16083d719b3b95",
    "validate-tests": "fe0ccaa869227e09ff5ad2a9de75ff530f77b620ffd8119f52a6b4447a6842bf",
    "reserve": "2a427f6dd305bca3a745650e4955c23749ae44a9df181d27a81c7635c341ab91",
    "verify": "bd53595c1e97fa2c8f3a2490a91c530ac9fa79c6d65fea628623f4f06b9419fb",
    "bind-review": "aefc537ed33125ac6b8b50a1e172e7381a855a2fc6eff29dfc4ffbbdc4d2840b",
    "verify-final": "3e49e7573f17ba9e4f9062a690dacc2f09d3be7d971af40ec96c3bedd5a7c530",
    "git-commit": "810f802267ee1e06947ff07babf17f9f5177fd14d319c551bdadc982f4d1403e",
    "git-merge": "47e661d1270ef5b5946a0f8edb875951db5a8adf5ba00a894c21e76f6a00efce",
}

INSTALLED_SCRIPTS = {
    "git-branch": "da2f5ee85829711c4204aea34d18c7a8739e2ba8e6937fe9883bedb130c2e6b8",
    "validate-spec": "5b1ef088c7d11702f18e54404104d1c6f93a55cf3c0156cd18c0817a4cc55a37",
    "validate-tests": "5cf5b6ac2e9af44bd177bffbe78f2296af3c67bcd247009c7591681a13955954",
    "git-commit": "b388b11032201c9b8841a7e8e901ded296fa8b0935f89f495574ac82f03ef331",
    "git-merge": "33a0b75f73b7b3301d9463de8a26f06e118602df47eb2f95e4512079a98d46c2",
}

INSTRUCTIONS = {
    "spec": "Declare cada critério em uma linha própria como '- CA001: descrição'. Use um número positivo único. Referências no restante do texto podem citar o código.",
    "tests": (
        "Entregue tests/ambiente.json atualizado nesta tentativa. Declare os como any, linux, windows ou darwin, "
        "commands com os executáveis necessários, env com os nomes das variáveis que podem ter valor vazio, "
        "envNonempty com os nomes que exigem valor e inputs com os caminhos relativos ao workspace de arquivos "
        "externos já materializados e não vazios. Declare interactive como false para suítes automáticas. "
        "Use listas vazias quando aplicável. Registre os nomes das variáveis e preserve os valores no ambiente. "
        "Quando necessário, declare minimumVersions como um objeto que associa um comando de commands à versão "
        "mínima numérica, por exemplo {\"rustc\":\"1.95\"}. Verifique o sistema em que os casos realmente rodam. "
        "As duas suítes devem executar com stdin fechado e retornar 1 para regressões do código e 78 "
        "nas falhas de pré-requisito. "
        "Registre como provisionar os requisitos em testes.md. Confira a sintaxe com sh -n e execute as suítes "
        "nesta etapa somente quando o ambiente e os dados já estiverem disponíveis."
    ),
    "implement": (
        "Prepare no índice do Git todos os arquivos de produto da candidata antes de entregar ao I5. "
        "Mantenha os artefatos de controle da SOP e as suítes de aceite fora das alterações preparadas."
    ),
    "review": (
        "Revise a árvore candidata testada indicada em .sop/review-tree. Confira com git write-tree no "
        "repositório e registre uma única linha 'Reviewed-Git-Tree: <hash>' em review.md. "
        "Preserve o conteúdo e o índice do Git durante a revisão."
    ),
    "fix": (
        "Ajuste somente os achados confirmados da revisão atual. Preserve spec.md, testes.md, as suítes de "
        "aceite e review.md. Prepare no índice do Git todos os ajustes de produto. Se a revisão estiver limpa, "
        "entregue a mesma árvore. Uma árvore alterada passa novamente por revisão antes da entrega."
    ),
}

CASE_PROMPT = (
    "Em cada script declare uma linha '# SOP-CASE: identificador' por caso. Use identificadores únicos "
    "na suíte com os códigos CA correspondentes, como test_CA001_visible. A união precisa cobrir toda "
    "a especificação. Quando SOP_SUITE_RECEIPT estiver definido, grave nesse caminho um JSON com "
    "nonce igual a SOP_SUITE_NONCE e cases como objeto de identificador para resultado. Extraia os "
    "resultados do executor real. Use pass somente para casos executados e aprovados e registre "
    "casos ignorados ou falhos como skip ou fail."
)

SHELL_PREFIX = r'''sop_guard() {
    sop_python=python3
    if ! "$sop_python" -c 'import sys; sys.exit(sys.version_info < (3, 9))' </dev/null >/dev/null 2>&1; then
        sop_python=python
        if ! "$sop_python" -c 'import sys; sys.exit(sys.version_info < (3, 9))' </dev/null >/dev/null 2>&1; then
            printf '%s\n' 'SOP bloqueado. Instale Python 3.9 ou posterior no executor.' >&2
            return 78
        fi
    fi
    sop_status=0
    PYTHONIOENCODING=utf-8 "$sop_python" -X utf8 - "$SOP_ROOT" "$@" <<'SOP_RUNTIME' || sop_status=$?
'''

COMMON = r'''import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import stat
import subprocess
import sys
import tempfile

ROOT = Path(sys.argv[1]).resolve()
CONTROL = ROOT / ".sop"
STORE = CONTROL / "workflow-state"
RUNTIME = ROOT / ".agentflow"
RECOVERY = "Restaure a reserva a partir da origem aprovada ou volte à validação e aprovação dos testes."
VISIBLE = ("spec.md", "testes.md", "verify.sh", "tests")
RESERVED = ("verify-reservado.sh", "tests-reservados")


class PrerequisiteError(Exception):
    pass


def interrupt(signum, frame):
    raise InterruptedError()


for signum in (signal.SIGINT, signal.SIGTERM, getattr(signal, "SIGHUP", signal.SIGTERM)):
    signal.signal(signum, interrupt)


def ordinary(path):
    info = path.lstat()
    if stat.S_ISLNK(info.st_mode) or getattr(info, "st_file_attributes", 0) & getattr(stat, "FILE_ATTRIBUTE_REPARSE_POINT", 0):
        raise PrerequisiteError("O estado da SOP precisa de arquivos e diretórios locais sem links.")
    return info


def digest(path):
    if not stat.S_ISREG(ordinary(path).st_mode):
        raise PrerequisiteError("Um arquivo da SOP deixou de ser regular.")
    checksum = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            checksum.update(chunk)
    return checksum.hexdigest()


def directory(path):
    path.mkdir(mode=0o700, exist_ok=True)
    if not stat.S_ISDIR(ordinary(path).st_mode):
        raise PrerequisiteError("O diretório de controle da SOP está indisponível.")


def store():
    directory(CONTROL)
    directory(STORE)


def record(path, data):
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=path.parent, delete=False) as file:
            temporary = Path(file.name)
            json.dump(data, file, sort_keys=True)
            file.flush()
            os.fsync(file.fileno())
        os.replace(temporary, path)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


def read(path, message):
    def unique_entries(entries):
        result = dict(entries)
        if len(result) != len(entries):
            raise ValueError()
        return result

    try:
        if not stat.S_ISREG(ordinary(path).st_mode):
            raise ValueError()
        data = json.loads(path.read_text(encoding="utf-8-sig"), object_pairs_hook=unique_entries)
        if not isinstance(data, dict):
            raise ValueError()
        return data
    except (OSError, ValueError, RecursionError):
        raise PrerequisiteError(message) from None


def latest_attempt(producer):
    runtime = RUNTIME / "runtime"
    pattern = re.compile(re.escape(producer) + r"-([1-9][0-9]*)-[1-9][0-9]*")
    for path in (RUNTIME, runtime):
        if not os.path.lexists(path):
            return 0
        if not stat.S_ISDIR(ordinary(path).st_mode):
            raise PrerequisiteError("O contexto da tentativa precisa de diretórios locais.")
    return max((int(match[1]) for path in runtime.iterdir()
                if (match := pattern.fullmatch(path.name)) and stat.S_ISDIR(ordinary(path).st_mode)), default=0)


def attempt_proof(producer, attempt):
    start = RUNTIME / "runtime" / f"{producer}-{attempt}-1" / "run-context.json"
    result = RUNTIME / "gate-results" / producer / f"attempt-{attempt}.json"
    verdict = read(result, "O resultado da tentativa atual está indisponível.")
    if not isinstance(verdict.get("decision"), str) or verdict["decision"].strip().lower() != "pass":
        raise PrerequisiteError("A tentativa atual precisa de um resultado aprovado pelo gate.")
    return [attempt, digest(start), digest(result)], ordinary(start).st_mtime_ns


def check_attempt(producer, token):
    if (not isinstance(token, list) or len(token) != 3 or type(token[0]) is not int or token[0] < 1
            or any(not isinstance(value, str) or not re.fullmatch(r"[0-9a-f]{64}", value) for value in token[1:])):
        raise PrerequisiteError("A evidência da tentativa está incompleta.")
    latest = latest_attempt(producer)
    if latest and (latest != token[0] or attempt_proof(producer, latest)[0] != token):
        raise PrerequisiteError("A evidência da tentativa mudou.")


def workspace_input(name):
    path = ROOT / name
    resolved = path.resolve()
    if not resolved.is_relative_to(ROOT) or ".agentflow" in (part.casefold() for part in resolved.relative_to(ROOT).parts):
        raise OSError()
    return path
'''

CRITERIA = r'''DECLARATION = re.compile(
    r"(?im)^[ \t]*(?:(?:#{1,6}|[-+*]|\d+[.)])[ \t]+|\|[ \t]*)?"
    r"(?:\[[ x]\][ \t]+)?(?:\*\*|__|\x60)?CA-?([0-9]+)(?![A-Za-z0-9])"
)
CASE_CODE = re.compile(r"(?i:(?<![a-z0-9])ca-?([0-9]+)(?![a-z0-9]))|CA-?([0-9]+)(?=[A-Z_]|[^\w]|$)")


def criterion_number(value):
    return value.lstrip("0") or "0"


def invalid_criterion(message):
    raise PrerequisiteError(message)


def declared_criteria(text, fail=invalid_criterion):
    numbers = [criterion_number(value) for value in DECLARATION.findall(text)]
    if not numbers:
        fail("declare pelo menos um critério de aceitação numérico, por exemplo CA001")
    if "0" in numbers or len(set(numbers)) != len(numbers):
        fail("use números CA positivos e únicos. Zeros à esquerda não criam um critério diferente")
    return set(numbers)


def case_criteria(text):
    return {criterion_number(first or second) for first, second in CASE_CODE.findall(text)}


def suite_contract():
    required = declared_criteria((ROOT / "spec.md").read_text(encoding="utf-8-sig"))
    suites = {}
    covered = set()
    for name in ("verify.sh", "verify-reservado.sh"):
        declarations = re.findall(r"(?m)^[ \t]*#[ \t]*SOP-CASE:[ \t]*([^\r\n]*)",
                                  (ROOT / name).read_text(encoding="utf-8-sig"))
        cases = [case.strip() for case in declarations]
        if not cases or len(cases) != len(set(cases)):
            raise PrerequisiteError(f"Declare casos SOP-CASE únicos em {name}.")
        for case in cases:
            criteria = case_criteria(case)
            if not re.fullmatch(r"[a-zA-Z0-9_.-]+", case) or not criteria or not criteria <= required:
                raise PrerequisiteError(f"Associe cada caso de {name} a CAs declarados em spec.md.")
            covered.update(criteria)
        suites[name] = sorted(cases)
    if covered != required:
        raise PrerequisiteError("As suítes precisam cobrir todos os CAs de spec.md.")
    return suites
'''

PREFLIGHT = r'''import platform

MANIFEST = ROOT / "tests/ambiente.json"


def version_tuple(value):
    numbers = tuple(map(int, value.split(".")))
    return numbers + (0,) * (3 - len(numbers))


def preflight():
    requirements = read(MANIFEST, "Forneça tests/ambiente.json válido antes de continuar.")
    fields = {"os", "commands", "env", "envNonempty", "inputs", "interactive"}
    if not fields <= requirements.keys() or requirements.keys() - fields - {"minimumVersions"}:
        raise PrerequisiteError("Declare os, commands, env, envNonempty, inputs e interactive em tests/ambiente.json.")
    system = requirements["os"]
    if system not in ("any", "linux", "windows", "darwin"):
        raise PrerequisiteError("Use any, linux, windows ou darwin em os no manifesto.")
    for key, pattern in (
        ("commands", r"[A-Za-z0-9_][A-Za-z0-9_.+-]*"),
        ("env", r"[A-Za-z_][A-Za-z0-9_]*"),
        ("envNonempty", r"[A-Za-z_][A-Za-z0-9_]*"),
        ("inputs", r"(?:[A-Za-z0-9_. -]+/)*[A-Za-z0-9_. -]+"),
    ):
        entries = requirements[key]
        if (not isinstance(entries, list)
                or any(not isinstance(value, str) or not re.fullmatch(pattern, value) for value in entries)
                or len(entries) != len(set(entries))):
            raise PrerequisiteError(f"Use uma lista de entradas válidas e únicas em {key} no manifesto.")
    if set(requirements["env"]) & set(requirements["envNonempty"]):
        raise PrerequisiteError("Declare cada variável em apenas uma lista do manifesto.")
    if requirements["interactive"] is not False:
        raise PrerequisiteError("A verificação exige suítes automatizadas. Declare interactive como false e forneça as entradas antes da execução.")

    minimums = requirements.get("minimumVersions", {})
    version_pattern = r"[0-9]{1,9}(?:\.[0-9]{1,9}){1,2}"
    if (not isinstance(minimums, dict) or set(minimums) - set(requirements["commands"])
            or any(not isinstance(value, str) or not re.fullmatch(version_pattern, value) for value in minimums.values())):
        raise PrerequisiteError("Declare versões mínimas numéricas apenas para comandos de commands.")
    actual = platform.system().lower()
    if system not in ("any", actual):
        raise PrerequisiteError(f"Sistema exigido: {system}. Executor atual: {actual}. Selecione um executor compatível.")

    commands = {name: shutil.which(name) for name in dict.fromkeys(("sh", "git", *requirements["commands"]))}
    problems = []
    missing = [name for name, location in commands.items() if not location]
    if missing:
        problems.append("programas fora do PATH: " + ", ".join(missing))
    missing = [name for name in requirements["env"] if name not in os.environ]
    if missing:
        problems.append("variáveis ausentes: " + ", ".join(missing))
    missing = [name for name in requirements["envNonempty"] if not os.environ.get(name, "").strip()]
    if missing:
        problems.append("variáveis que exigem valor: " + ", ".join(missing))
    for name in requirements["inputs"]:
        if any(part.strip() in ("", ".", "..") or part.casefold() == ".agentflow" for part in name.split("/")):
            raise PrerequisiteError("Use caminhos relativos ao workspace em inputs.")
        try:
            info = ordinary(workspace_input(name))
            if not stat.S_ISREG(info.st_mode) or not info.st_size:
                raise OSError()
        except OSError:
            problems.append(f"arquivo obrigatório ausente ou vazio: {name}")
    for name, minimum in minimums.items():
        if commands[name] is None:
            continue
        try:
            result = subprocess.run([commands[name], "--version"], cwd=ROOT, stdin=subprocess.DEVNULL,
                                    stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=10)
            version = re.search(rb"(?<![A-Za-z0-9.])[vV]?(" + version_pattern.encode() + rb")(?![0-9]|\.[0-9])", result.stdout)
            if result.returncode == 0 and version and version_tuple(version[1].decode()) >= version_tuple(minimum):
                continue
        except (OSError, subprocess.TimeoutExpired):
            pass
        problems.append(f"instale e confira {name} na versão {minimum} ou posterior")
    if problems:
        raise PrerequisiteError(" | ".join(problems))
    return commands["sh"]
'''

SEAL = r'''VALIDATION = STORE / "validation.json"
CURRENT_SEAL = STORE / "current-seal.json"


def inventory(root, names, partial=False):
    result = {}

    def visit(path):
        name = path.relative_to(root).as_posix()
        info = ordinary(path)
        if stat.S_ISDIR(info.st_mode):
            result[name] = None
            for child in sorted(path.iterdir()):
                visit(child)
        elif stat.S_ISREG(info.st_mode):
            result[name] = [digest(path), bool(info.st_mode & stat.S_IXUSR)]
        else:
            raise PrerequisiteError("A aprovação exige arquivos e diretórios regulares.")

    for name in names:
        if partial and not os.path.lexists(root / name):
            continue
        visit(root / name)
    return result


def input_hashes():
    manifest = read(ROOT / "tests/ambiente.json", "Forneça tests/ambiente.json válido.")
    return {name: digest(workspace_input(name)) for name in manifest["inputs"]}


def approved_inputs(token):
    return {"tests": token, "visible": inventory(ROOT, VISIBLE), "reserved": inventory(ROOT, RESERVED),
            "cases": suite_contract(), "inputs": input_hashes()}


def acceptance(attempt):
    token, _ = attempt_proof("tests", attempt)
    return approved_inputs(token)


def seal_path(selection):
    name = selection["directory"]
    if not isinstance(name, str) or not re.fullmatch(f"seal-{selection['tests'][0]}-[a-z0-9_]+", name):
        raise PrerequisiteError(RECOVERY)
    return STORE / name


def approved_seal():
    try:
        store()
        selected = read(CURRENT_SEAL, RECOVERY)
        token = selected["tests"]
        check_attempt("tests", token)
        sealed = seal_path(selected)
        if not stat.S_ISDIR(ordinary(sealed).st_mode):
            raise OSError()
        payload = read(sealed / "approval.json", RECOVERY)
        if (selected["seal"] != digest(sealed / "approval.json") or payload["tests"] != token
                or payload["reserved"] != inventory(sealed, RESERVED)):
            raise PrerequisiteError(RECOVERY)
        if os.path.lexists(VALIDATION) and read(VALIDATION, RECOVERY) != payload:
            raise PrerequisiteError(RECOVERY)
    except (KeyError, OSError, PrerequisiteError):
        raise PrerequisiteError("Reserva ausente, desatualizada ou inválida. " + RECOVERY) from None
    if payload.get("visible") != inventory(ROOT, VISIBLE):
        raise PrerequisiteError("Os testes visíveis ou a especificação divergiram da aprovação. " + RECOVERY)
    if payload.get("inputs") != input_hashes():
        raise PrerequisiteError("Os insumos declarados divergiram da aprovação. " + RECOVERY)
    return sealed, payload


def remove_reserved():
    for name in RESERVED:
        path = ROOT / name
        if path.is_symlink():
            path.unlink()
        elif path.is_dir() and path.resolve().parent == ROOT:
            shutil.rmtree(path)
        elif path.is_dir():
            raise PrerequisiteError("O diretório temporário reservado aponta para fora do checkout.")
        else:
            path.unlink(missing_ok=True)


def publish_seal(payload):
    token = payload["tests"]
    check_attempt("tests", token)
    if payload["visible"] != inventory(ROOT, VISIBLE) or payload["inputs"] != input_hashes():
        raise PrerequisiteError("O material mudou desde a validação anterior à aprovação. " + RECOVERY)
    if payload["reserved"] != inventory(ROOT, RESERVED):
        raise PrerequisiteError("Os bytes reservados mudaram antes do congelamento. " + RECOVERY)
    prepared = Path(tempfile.mkdtemp(dir=STORE, prefix=f"seal-{token[0]}-"))
    try:
        shutil.copy2(ROOT / RESERVED[0], prepared / RESERVED[0])
        shutil.copytree(ROOT / RESERVED[1], prepared / RESERVED[1])
        check_attempt("tests", token)
        if payload != approved_inputs(token) or payload["reserved"] != inventory(prepared, RESERVED):
            raise PrerequisiteError("O material mudou durante o congelamento. " + RECOVERY)
        record(prepared / "approval.json", payload)
        seal = digest(prepared / "approval.json")
    except (OSError, PrerequisiteError):
        shutil.rmtree(prepared, ignore_errors=True)
        raise
    # Publish the pointer atomically because a Windows-hosted bind can deny directory renames.
    record(CURRENT_SEAL, {"tests": token, "directory": prepared.name, "seal": seal})


def reserve():
    store()
    if os.path.lexists(VALIDATION):
        payload = read(VALIDATION, RECOVERY)
        previous = read(CURRENT_SEAL, RECOVERY) if os.path.lexists(CURRENT_SEAL) else None
        if previous is None or previous["tests"][0] < payload["tests"][0]:
            publish_seal(payload)
    _, payload = approved_seal()
    present = inventory(ROOT, RESERVED, partial=True)
    if any(key not in payload["reserved"] or payload["reserved"][key] != value for key, value in present.items()):
        raise PrerequisiteError("A reserva existente não corresponde ao material aprovado. " + RECOVERY)
    remove_reserved()
    print("I3: reserva conferida e selada na área de estado da SOP.")
'''

REPOSITORY = r'''def repository():
    if (ROOT / ".git").exists():
        return ROOT
    matches = []
    for path in ROOT.iterdir():
        if not path.is_dir() or path.is_symlink():
            continue
        marker = path / ".git"
        if marker.is_file():
            try:
                git(path, "rev-parse", "--resolve-git-dir", ".git")
            except subprocess.CalledProcessError:
                continue
        elif not marker.is_dir():
            continue
        matches.append(path)
    if len(matches) != 1:
        raise PrerequisiteError("A SOP precisa de um checkout Git identificável.")
    return matches[0]


def git(repo, *args):
    result = subprocess.run(["git", "-C", str(repo), *args], stdin=subprocess.DEVNULL,
                            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, check=True)
    return result.stdout
'''


BOUND_BRANCH = r'''manifest = os.environ.get("AGENTFLOW_WORK_BRANCHES")
scope = os.environ.get("AGENTFLOW_EXECUTION_SCOPE", "run" if manifest is not None else "repositories")
workspace = Path(sys.argv[2]).resolve()
invalid = "I0 precisa de um vínculo válido com uma única branch de trabalho e sua base."
if scope == "run":
    fields = (manifest or "").split("\t")
    if len(fields) != 3 or "\n" in (manifest or "") or workspace != ROOT:
        raise PrerequisiteError(invalid)
    location, branch, base = fields
    if not location:
        raise PrerequisiteError(invalid)
elif scope == "repositories":
    location = os.environ.get("AGENTFLOW_REPOSITORY_DIR") or "."
    branch = os.environ.get("AGENTFLOW_WORK_BRANCH")
    base = os.environ.get("AGENTFLOW_BASE_BRANCH")
else:
    raise PrerequisiteError(invalid)

checkout = Path(location)
if (not branch or not base or branch == base or checkout.drive
        or checkout.is_absolute() or ".." in checkout.parts):
    raise PrerequisiteError(invalid)
repo = (workspace / checkout).resolve()
if (not repo.is_relative_to(workspace) or repo != (repository() if scope == "run" else ROOT)
        or Path(git(repo, "rev-parse", "--show-toplevel").decode().rstrip("\n")).resolve() != repo):
    raise PrerequisiteError("O checkout diverge do repositório vinculado ao I0.")
if git(repo, "branch", "--show-current").decode().rstrip("\n") != branch:
    raise PrerequisiteError("O checkout diverge da branch de trabalho vinculada ao I0.")
head = git(repo, "rev-parse", "--short", "HEAD").decode().strip()
if git(repo, "status", "--porcelain", "--untracked-files=all"):
    raise PrerequisiteError("O checkout tem alterações locais. Preserve-as antes de iniciar o I0.")
paths = {repo / ".sop/git-branch": branch, repo / ".sop/git-base": base}
if git(repo, "ls-files", "--", ".sop/git-branch", ".sop/git-base"):
    raise PrerequisiteError("Mantenha o vínculo da SOP fora do índice do Git.")
base_ref = "refs/heads/" + base
try:
    git(repo, "check-ref-format", base_ref)
    git(repo, "remote", "get-url", "origin")
    git(repo, "fetch", "--quiet", "--no-tags", "--no-prune", "--no-recurse-submodules",
        "--refmap=", "origin", base_ref)
    git(repo, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
except subprocess.CalledProcessError as exc:
    raise PrerequisiteError("Não foi possível confirmar a base vinculada ao I0 no remoto origin.") from exc
directory(repo / ".sop")
for path in paths:
    if (path.exists() or path.is_symlink()) and not stat.S_ISREG(ordinary(path).st_mode):
        raise PrerequisiteError("O vínculo da SOP precisa de arquivos regulares.")
for path, value in paths.items():
    path.write_bytes((value + "\n").encode("utf-8"))
print(f"BRANCH_NOVA={branch} BASE={base} ({head})")
'''


GIT = r'''REPORT = ROOT / "review.md"
REVIEW_INPUT = STORE / "review-input.json"
REVIEW_PROOF = STORE / "review-proof.json"
FINAL_PROOF = STORE / "final-proof.json"
DELIVERY_PROOF = STORE / "delivery.json"
OID = r"(?:[0-9a-f]{40}|[0-9a-f]{64})"


''' + REPOSITORY + r'''

def git_oid(repo, *args):
    oid = git(repo, *args).decode("ascii").strip()
    if not re.fullmatch(OID, oid):
        raise PrerequisiteError("O Git não forneceu um identificador de árvore ou commit válido.")
    return oid


def git_tree():
    repo = repository()
    metadata = [".sop", ".agentflow"]
    if repo == ROOT:
        if git(repo, "ls-files", "-z", "--", STORE.relative_to(ROOT).as_posix(), *RESERVED):
            raise PrerequisiteError("Mantenha a reserva e a evidência da SOP fora do índice do Git.")
        metadata.extend((*VISIBLE, *RESERVED, "plan.md", "implementation.md", "review.md"))
    scope = ["--", ".", *(f":(top,exclude,literal){name}" for name in metadata)]
    if git(repo, "diff", "--name-only", "-z", "--ignore-submodules=none", *scope) or git(
        repo, "ls-files", "--others", "--exclude-standard", "-z", *scope
    ):
        raise PrerequisiteError("Prepare no índice do Git a árvore completa do produto antes de verificar ou entregar.")
    return git_oid(repo, "write-tree")


def review_input(tree, seal, suites):
    directory(CONTROL)
    file = CONTROL / "review-tree"
    if os.path.lexists(file):
        ordinary(file)
    file.write_text(tree + "\n", encoding="ascii")
    before = latest_attempt("review")
    if os.path.lexists(REVIEW_PROOF):
        prior = read(REVIEW_PROOF, "A evidência da revisão mudou.")
        before = max(before, prior["attempt"][0])
    record(REVIEW_INPUT, {"tree": tree, "seal": seal, "before": before, "suites": suites})


def check_report(target, bound=None):
    if bound is None:
        proof, started = attempt_proof("review", latest_attempt("review"))
        if ordinary(REPORT).st_mtime_ns < started:
            raise PrerequisiteError("Execute a revisão atual e gere review.md para a árvore testada.")
    else:
        proof = bound["attempt"]
        check_attempt("review", proof)
    if proof[0] <= target["before"] or not stat.S_ISREG(ordinary(REPORT).st_mode):
        raise PrerequisiteError("Execute a revisão atual e gere review.md para a árvore testada.")
    tree = target["tree"]
    raw = REPORT.read_bytes()
    report = raw.decode("utf-8-sig")
    if [line.strip() for line in report.splitlines() if line.startswith("Reviewed-Git-Tree:")] != [
        "Reviewed-Git-Tree: " + tree
    ]:
        raise PrerequisiteError("Registre a árvore Git testada em review.md e execute a revisão dessa árvore.")
    return {"tree": tree, "attempt": proof, "report": hashlib.sha256(raw).hexdigest(),
            "input": digest(REVIEW_INPUT)}


def suite_proofs_valid(proofs, cases):
    return isinstance(proofs, dict) and proofs.keys() == cases.keys() and all(
        isinstance(proof, dict) and proof.keys() == {"nonce", "sha256", "passed"}
        and proof["passed"] == len(cases[name])
        and all(isinstance(proof[field], str) and re.fullmatch(r"[0-9a-f]{64}", proof[field])
                for field in ("nonce", "sha256"))
        for name, proof in proofs.items()
    )


def review_target(seal, cases):
    target = read(REVIEW_INPUT, "Execute a verificação que prepara a árvore para revisão.")
    if target["seal"] != seal or not suite_proofs_valid(target.get("suites"), cases):
        raise PrerequisiteError("A aprovação dos testes mudou. Verifique a árvore antes de revisar.")
    return target


def bind_review():
    sealed, payload = approved_seal()
    target = review_target(digest(sealed / "approval.json"), payload["cases"])
    if target["tree"] != git_tree():
        raise PrerequisiteError("A árvore mudou durante a revisão. Volte à verificação e revise a nova árvore.")
    if not latest_attempt("review"):
        current_review(target["seal"], payload["cases"])
    else:
        record(REVIEW_PROOF, check_report(target))
    print("Revisão vinculada à árvore Git testada.")


def current_review(seal, cases):
    target = review_target(seal, cases)
    proof = read(REVIEW_PROOF, "Vincule a revisão atual antes de entregar.")
    try:
        actual = check_report(target, proof)
    except PrerequisiteError as exc:
        raise PrerequisiteError("A evidência da revisão mudou. " + str(exc)) from None
    if proof != actual:
        raise PrerequisiteError("A evidência da revisão mudou. Execute novamente a revisão da árvore.")
    return proof


def delivery_gate(action):
    sealed, payload = approved_seal()
    seal = digest(sealed / "approval.json")
    reviewed = current_review(seal, payload["cases"])
    final = read(FINAL_PROOF, "Execute a verificação final da árvore revisada.")
    if (final.get("review") != reviewed or final.get("seal") != seal or reviewed["tree"] != git_tree()
            or not suite_proofs_valid(final.get("suites"), payload["cases"])):
        raise PrerequisiteError("A árvore ou a evidência mudou depois da verificação final. Verifique e revise a candidata atual.")
    if action == "before-commit":
        return
    repo = repository()
    head = git_oid(repo, "rev-parse", "HEAD")
    if reviewed["tree"] != git_oid(repo, "rev-parse", "HEAD^{tree}"):
        raise PrerequisiteError("O commit não contém a árvore testada e revisada. Confira a candidata antes de publicar.")
    if action == "before-push":
        branch, base, base_sha, push_head = sys.argv[3:]
        if head != push_head:
            raise PrerequisiteError("O commit mudou antes da publicação. Confira o commit da candidata.")
        record(DELIVERY_PROOF, dict(final=final, branch=branch, base=base, head=head, base_sha=base_sha))
        return
    control = repo / ".sop"
    branch, base, approved_head, approved_base = (
        (control / name).read_text(encoding="utf-8").strip()
        for name in ("git-branch", "git-base", "git-approved-head", "git-approved-base")
    )
    expected = dict(final=final, branch=branch, base=base, head=head, base_sha=approved_base)
    if approved_head != head or expected != read(DELIVERY_PROOF, "O commit publicado precisa da evidência da entrega."):
        raise PrerequisiteError("As referências de entrega divergiram do commit verificado. Confira a publicação e a aprovação.")
'''

VERIFY = r'''def suite(shell, name, output, cases):
    with tempfile.TemporaryDirectory(dir=RUNTIME, prefix="sop-suite-") as temporary:
        receipt = Path(temporary) / "receipt.json"
        nonce = secrets.token_hex(32)
        environment = {**os.environ, "PYTHONDONTWRITEBYTECODE": "1",
                       "SOP_SUITE_RECEIPT": str(receipt), "SOP_SUITE_NONCE": nonce}
        result = subprocess.run([shell, name], cwd=ROOT, stdin=subprocess.DEVNULL, stdout=output,
                                stderr=subprocess.STDOUT, env=environment).returncode
        if result in (2, 78, 126, 127) or result < 0 or result >= 128:
            raise PrerequisiteError(f"A suíte {name} informou falha de execução ({result}). Confira os pré-requisitos.")
        if result != 0:
            print(f"Não passou a suíte {name}. Confira a evidência da verificação.", file=sys.stderr)
            raise SystemExit(1)
        data = read(receipt, f"A suíte {name} não produziu resultado da execução atual.")
        if data != {"nonce": nonce, "cases": dict.fromkeys(cases, "pass")}:
            raise PrerequisiteError(f"O resultado de {name} precisa comprovar todos os casos da execução atual.")
        return {"nonce": nonce, "sha256": digest(receipt), "passed": len(cases)}


def verify(final):
    sealed, payload = approved_seal()
    seal = digest(sealed / "approval.json")
    FINAL_PROOF.unlink(missing_ok=True)
    exposed = STORE / "exposed.json"
    if os.path.lexists(exposed):
        if read(exposed, RECOVERY) != {"seal": seal}:
            raise PrerequisiteError("A exposição temporária não corresponde ao selo atual. " + RECOVERY)
        remove_reserved()
        exposed.unlink()
    if any(os.path.lexists(ROOT / name) for name in RESERVED):
        raise PrerequisiteError("Há arquivos reservados no checkout sem registro da verificação. " + RECOVERY)

    shell = preflight()
    reviewed = current_review(seal, payload["cases"]) if final else None
    tree = git_tree()
    directory(RUNTIME)
    log = RUNTIME / "sop-verify.log"
    if os.path.lexists(log):
        ordinary(log)
    with log.open("w", encoding="utf-8") as output:
        visible = suite(shell, "verify.sh", output, payload["cases"]["verify.sh"])
    if any(os.path.lexists(ROOT / name) for name in RESERVED):
        raise PrerequisiteError("A suíte visível criou arquivos com nomes reservados. " + RECOVERY)
    record(exposed, {"seal": seal})
    try:
        shutil.copy2(sealed / RESERVED[0], ROOT / RESERVED[0])
        shutil.copytree(sealed / RESERVED[1], ROOT / RESERVED[1])
        if inventory(ROOT, RESERVED) != payload["reserved"]:
            raise PrerequisiteError("Os insumos reservados mudaram durante a verificação. " + RECOVERY)
        reserved = suite(shell, "verify-reservado.sh", subprocess.DEVNULL, payload["cases"]["verify-reservado.sh"])
    finally:
        remove_reserved()
        exposed.unlink(missing_ok=True)

    if tree != git_tree() or payload != approved_seal()[1]:
        raise PrerequisiteError("A árvore ou os insumos mudaram durante os testes. Execute novamente a verificação.")
    suites = {"verify.sh": visible, "verify-reservado.sh": reserved}
    if not final or reviewed["tree"] != tree:
        review_input(tree, seal, suites)
        if final:
            print("I5 final: a árvore ajustada passou nos testes e precisa de uma nova revisão.", file=sys.stderr)
            raise SystemExit(79)
    else:
        record(FINAL_PROOF, {"review": reviewed, "seal": seal, "suites": suites})
    print("Verificação das suítes concluída para a árvore " + tree + ".")
'''

END = r'''except PrerequisiteError as exc:
    print("SOP bloqueado: " + str(exc), file=sys.stderr)
    sys.exit(78)
except (OSError, ValueError, KeyError, TypeError, RecursionError, subprocess.SubprocessError):
    print("SOP bloqueado. Confira ferramentas, arquivos e espaço do executor. " + RECOVERY, file=sys.stderr)
    sys.exit(78)
SOP_RUNTIME
    case "$sop_status" in
        0|1|78|79) return "$sop_status" ;;
        *) printf '%s\n' 'SOP bloqueado. Confira a execução do Python no executor.' >&2; return 78 ;;
    esac
}
'''

def shell_guard(libraries, body):
    lines = "\n".join("    " + line if line else "" for line in body.rstrip().splitlines())
    return SHELL_PREFIX + COMMON + libraries + "\n\ntry:\n" + lines + "\n" + END


def shell_stage(libraries, body):
    return 'set -eu\nSOP_ROOT="$PWD"\n' + shell_guard(libraries, body) + "sop_guard\n"


def replace_once(text, marker, replacement):
    if text.count(marker) != 1:
        raise ValueError("Expected a unique supported SOP entry point.")
    return text.replace(marker, replacement, 1)


def spec_stage(script):
    body = replace_once(script, "\nprint(", '\ndeclared_criteria(Path("spec.md").read_text(encoding="utf-8-sig"), fail)\nprint(')
    return shell_stage(CRITERIA, body)


def tests_stage(script):
    body = replace_once(script, LEGACY["PREFLIGHT"], "")
    body = replace_once(body, "for filename in REQUIRED_FILES:", """preflight()
if ordinary(MANIFEST).st_mtime_ns < start.st_mtime_ns:
    raise PrerequisiteError("Entregue tests/ambiente.json da tentativa atual.")
snapshot = acceptance(attempt)
for filename in REQUIRED_FILES:""")
    body = replace_once(body, "scope = codes(", "scope = declared_criteria(")
    body = replace_once(body, "if codes(name) & scope:", "if case_criteria(name) & scope:")
    body = replace_once(body, "\nprint(", """
if snapshot != acceptance(attempt):
    raise PrerequisiteError("Os insumos mudaram durante a validação dos testes. Valide novamente.")
store()
record(STORE / "validation.json", snapshot)
print(""")
    return shell_stage(CRITERIA + PREFLIGHT + SEAL, body)


def git_branch_stage(script):
    return ('set -eu\nSOP_ROOT="$PWD"\n' + shell_guard(REPOSITORY, BOUND_BRANCH)
            + 'if [ "${AGENTFLOW_EXECUTION_SCOPE+x}${AGENTFLOW_WORK_BRANCHES+x}${AGENTFLOW_WORK_BRANCH+x}${AGENTFLOW_BASE_BRANCH+x}" ]; then\n'
            + '    sop_guard "${AGENTFLOW_WORKSPACE-$PWD}"\n    exit\nfi\n' + script)


def git_stage(script, commit):
    guard = shell_guard(SEAL + GIT, "delivery_gate(sys.argv[2])")
    action = "before-commit" if commit else "before-merge"
    body = replace_once(script, 'SOP_ROOT="$PWD"\n', 'SOP_ROOT="$PWD"\n' + guard + "sop_guard " + action + "\n")
    if commit:
        anchor = 'git push origin "$HEAD_SHA:refs/heads/$BR"'
        body = replace_once(body, anchor, 'sop_guard before-push "$BR" "$BASE" "$BASE_SHA" "$HEAD_SHA"\n' + anchor)
    else:
        body = merge_stage(body)
    return body


MERGE_STATE = r'''
store()
identity = {"repo": os.environ["SLUG"].casefold(), "head": approved_head, "base_sha": approved_base,
            "branch": br, "base": base}
attempt_key = hashlib.sha256(json.dumps(identity, sort_keys=True).encode()).hexdigest()
attempt_path = STORE / f"merge-{attempt_key}.json"
attempt = read(attempt_path, "A tentativa de merge está ilegível.") if os.path.lexists(attempt_path) else None
if attempt is not None:
    number = attempt.get("number")
    if type(number) is not int or number < 1 or attempt != dict(identity, number=number):
        fail("A tentativa de merge não corresponde ao PR aprovado.")


def confirm_merge(sha):
    if not isinstance(sha, str) or not re.fullmatch(r"[0-9a-f]{40}", sha):
        fail("O provedor não confirmou o commit do merge.")
    status, commit = call("GET", "/git/commits/" + sha)
    if (status != 200 or commit.get("sha") != sha
            or [parent.get("sha") for parent in commit.get("parents", [])] != [approved_base, approved_head]):
        fail("O commit do merge não corresponde às duas referências aprovadas.")
'''


def merge_stage(script):
    opener = "python3 - <<'PY'\n"
    launcher = 'PYTHONIOENCODING=utf-8 "$sop_python" -X utf8 - "$SOP_ROOT" <<\'PY\'\n'
    script = replace_once(script, opener, launcher + COMMON)
    select = script.index("\ncheck_refs()\n")
    poll = script.index("\nBAD = ", select)
    script = script[:select] + MERGE_STATE + "\n\nif attempt is None:\n" + indent(script[select:poll].strip(), "    ") + "\n" + script[poll:]
    merged = script.index('\n    if pr.get("merged"):\n', poll)
    open_checks = script.index("\n    check_refs()\n", merged)
    script = script[:merged] + '''
    if (pr.get("number") != number or pr["head"].get("ref") != br
            or any((pr[side].get("repo") or {}).get("full_name", "").casefold() != identity["repo"]
                   for side in ("base", "head"))):
        fail("A identidade do PR divergiu da tentativa aprovada.")
    if pr.get("merged") is True:
        if attempt is None:
            fail("O PR foi concluído sem um registro de tentativa da SOP.")
        confirm_merge(pr.get("merge_commit_sha"))
        print(f"PR #{number}: merge já confirmado para as referências aprovadas.")
        sys.exit(0)
''' + script[open_checks:]
    request = '        status, res = call("PUT", "/pulls/%d/merge" % number, '
    script = replace_once(script, request, '''        if type(number) is not int or number < 1:
            fail("O provedor não identificou o PR aprovado.")
        attempt = dict(identity, number=number)
        record(attempt_path, attempt)
        try:
    ''' + request)
    success = '        if status == 200 and res.get("merged"):\n'
    return replace_once(script, success, '''        except OSError:
            fail("A resposta do merge se perdeu. Repita a etapa para consultar o mesmo PR.")
''' + success + '            confirm_merge(res.get("sha"))\n')


def fingerprint(script):
    return sha256(script.encode("utf-8")).hexdigest()


def expected_edges(chain):
    return {(source, target, "success") for source, target in zip(chain, chain[1:])}


def check_graph(workflow, nodes, installed):
    chain = CHAIN
    if installed:
        i = chain.index("fix")
        chain = chain[:i] + ("bind-review",) + chain[i:]
    edges = workflow["edges"]
    endpoints = {(edge["source"], edge["target"], edge.get("when")) for edge in edges}
    if (workflow.get("singleRepository") is not True or set(nodes) != set(chain)
            or len(edges) != len(endpoints) or endpoints != expected_edges(chain)
            or len({edge["id"] for edge in edges}) != len(edges)):
        raise ValueError("Expected the supported single-repository SOP graph.")
    for name in chain:
        expected = ("approval" if name.startswith("approve-")
                    else "script" if name in SOURCE_SCRIPTS or name == "bind-review" else "agent")
        if nodes[name]["type"] != expected:
            raise ValueError(f"Unexpected node type at {name}.")
    for name, target, reworks in (("approve-tests", "spec", 1), ("approve-merge", "fix", 1), ("verify", "implement", 2)):
        if nodes[name].get("rejectTo") != target or nodes[name].get("maxReworks") != reworks:
            raise ValueError(f"Expected the existing approval or rejection route at {name}.")
    if nodes["verify"].get("onFailure") != "reject":
        raise ValueError("Expected candidate rework at verify.")


def instructions(name, node, previous=False):
    prior = node.get("taskPrompt", "")
    if name == "tests":
        prior = prior.removesuffix(LEGACY["TEST_PROMPT"])
    return "\n\n".join(filter(None, (prior, INSTRUCTIONS[name], CASE_PROMPT if name == "tests" and not previous else "")))


def previous_body(script):
    return dedent(script.rsplit("\n\ntry:\n", 1)[1].split(END, 1)[0])


def previous_git(script, commit):
    stage = "before-commit" if commit else "before-merge"
    before, after = script.split(f"\nsop_guard {stage}\n", 1)
    marker = 'SOP_ROOT="$PWD"\n'
    source = before.split(marker, 1)[0] + marker + after
    if commit:
        return replace_once(source, 'sop_guard before-push "$BR" "$BASE" "$BASE_SHA" "$HEAD_SHA"\n', "")
    return replace_once(source, 'PYTHONIOENCODING=utf-8 "$sop_python" -X utf8 - <<\'PY\'', "python3 - <<'PY'")


def upgrade_scripts(nodes):
    spec = replace_once(previous_body(nodes["validate-spec"]["script"]),
                        'declared_criteria(Path("spec.md").read_text(encoding="utf-8-sig"))',
                        'declared_criteria(Path("spec.md").read_text(encoding="utf-8-sig"), fail)')
    return {
        "validate-spec": shell_stage(CRITERIA, spec),
        "validate-tests": shell_stage(CRITERIA + PREFLIGHT + SEAL, previous_body(nodes["validate-tests"]["script"])),
        "git-commit": git_stage(previous_git(nodes["git-commit"]["script"], commit=True), commit=True),
        "git-merge": git_stage(previous_git(nodes["git-merge"]["script"], commit=False), commit=False),
    }


def apply_reliability(workflow):
    nodes = {node["id"]: node for node in workflow["nodes"]}
    if len(nodes) != len(workflow["nodes"]):
        raise ValueError("Duplicate SOP node IDs.")
    installed = "bind-review" in nodes
    check_graph(workflow, nodes, installed)
    generated = {
        "reserve": shell_stage(CRITERIA + SEAL, "reserve()"),
        "verify": shell_stage(PREFLIGHT + SEAL + GIT + VERIFY, "verify(False)"),
        "bind-review": shell_stage(SEAL + GIT, "bind_review()"),
        "verify-final": shell_stage(PREFLIGHT + SEAL + GIT + VERIFY, "verify(True)"),
    }
    final_policy = {"onFailure": "reject", "rejectTo": "review", "maxReworks": 1}
    if installed:
        previous = fingerprint(nodes["reserve"]["script"]) == PREVIOUS_SCRIPTS["reserve"]
        unbound = fingerprint(nodes["git-branch"]["script"]) == SOURCE_SCRIPTS["git-branch"]
        expected = {**INSTALLED_SCRIPTS, **{name: fingerprint(code) for name, code in generated.items()}}
        if expected.keys() != (SOURCE_SCRIPTS.keys() | {"bind-review"}):
            raise ValueError("Incomplete published SOP script fingerprints.")
        current = PREVIOUS_SCRIPTS if previous else expected
        if unbound and not previous:
            current = {**current, "git-branch": SOURCE_SCRIPTS["git-branch"]}
        check_scripts(nodes, current, installed=True)
        check_manifest(nodes)
        if any(nodes[name].get("agentInstructions") != instructions(name, nodes[name], previous) for name in INSTRUCTIONS):
            raise ValueError("Expected active SOP agent instructions.")
        if any(nodes["verify-final"].get(key) != value for key, value in final_policy.items()):
            raise ValueError("Expected bounded re-review of the final tree.")
        if previous:
            generated.update(upgrade_scripts(nodes))
            for name, script in generated.items():
                nodes[name]["script"] = script
            nodes["tests"]["agentInstructions"] = instructions("tests", nodes["tests"])
        if unbound:
            nodes["git-branch"]["script"] = git_branch_stage(nodes["git-branch"]["script"])
        if previous or unbound:
            check_scripts(nodes, expected, installed=True)
        return

    if any(edge["id"] == "bind-review-fix" for edge in workflow["edges"]):
        raise ValueError("The SOP already uses the edge ID for the review binding.")
    if any(node.get("agentInstructions") or node.get("nonRetryableExitCodes") for node in nodes.values()):
        raise ValueError("The SOP reliability update is only partially applied.")
    task_prompt = nodes["tests"]["taskPrompt"]
    LEGACY["apply_preflight"](workflow)
    nodes["tests"]["taskPrompt"] = task_prompt
    check_scripts(nodes, SOURCE_SCRIPTS, installed=False)
    check_manifest(nodes)
    nodes["git-branch"]["script"] = git_branch_stage(nodes["git-branch"]["script"])
    generated.update({
        "validate-spec": spec_stage(nodes["validate-spec"]["script"]),
        "validate-tests": tests_stage(nodes["validate-tests"]["script"]),
        "git-commit": git_stage(nodes["git-commit"]["script"], commit=True),
        "git-merge": git_stage(nodes["git-merge"]["script"], commit=False),
    })
    for name in INSTRUCTIONS:
        nodes[name]["agentInstructions"] = instructions(name, nodes[name])
    position = {axis: (nodes["review"]["position"][axis] + nodes["fix"]["position"][axis]) / 2 for axis in ("x", "y")}
    bind = {"id": "bind-review", "name": "Vincular revisão à árvore testada", "type": "script",
            "position": position, "maxAttempts": 1, "maxReworks": 0, "onFailure": "fail", "metricStage": "testing"}
    nodes["bind-review"] = bind
    workflow["nodes"].insert(workflow["nodes"].index(nodes["fix"]), bind)
    for edge in workflow["edges"]:
        if (edge["source"], edge["target"]) == ("review", "fix"):
            edge["target"] = "bind-review"
            break
    workflow["edges"].append({"id": "bind-review-fix", "source": "bind-review", "target": "fix", "when": "success"})
    nodes["verify-final"].update(final_policy)
    for name, script in generated.items():
        nodes[name].update(script=script, scriptLanguage="shell", nonRetryableExitCodes=stop_codes(name))


def stop_codes(name):
    return [1, 78] if name == "verify-final" else [78]


def check_scripts(nodes, expected, installed):
    for name, digest in expected.items():
        node = nodes[name]
        language = "python" if not installed and name.startswith("validate-") else "shell"
        scopes = ("", "run", "repositories") if name == "git-branch" else ("", "run")
        if (node.get("scriptLanguage") != language or fingerprint(node["script"]) != digest
                or node.get("executionScope", "") not in scopes or node.get("maxAttempts") != 1):
            raise ValueError(f"Unsupported or partially patched SOP entry point at {name}.")
        if installed and name != "git-branch" and node.get("nonRetryableExitCodes") != stop_codes(name):
            raise ValueError(f"Unexpected exit policy at {name}.")


def check_manifest(nodes):
    manifest = LEGACY["MANIFEST_OUTPUT"]
    matching = [item for item in nodes["tests"]["artifactOutputs"]
                if item.get("path") == manifest["path"] or item.get("name") == manifest["name"]]
    if matching != [manifest]:
        raise ValueError("Expected a single required runtime manifest output.")


def write_export(output, workflow):
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", newline="\n", dir=output.parent,
                                         delete=False) as stream:
            temporary = Path(stream.name)
            json.dump(workflow, stream, ensure_ascii=False, indent=2)
            stream.write("\n")
        temporary.replace(output)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("workflow", type=Path, help="exported workflow JSON")
    parser.add_argument("output", type=Path, help="path for the derived workflow JSON")
    args = parser.parse_args()
    try:
        if args.workflow.resolve() == args.output.resolve():
            raise ValueError("Use a different path for the derived workflow.")
        workflow = json.loads(args.workflow.read_text(encoding="utf-8-sig"))
        apply_reliability(workflow)
        write_export(args.output, workflow)
    except (OSError, ValueError, KeyError, TypeError, AttributeError, RecursionError) as exc:
        message = str(exc) if type(exc) is ValueError else "Could not read, transform, or write the SOP export."
        parser.exit(1, f"{message}\n")


if __name__ == "__main__":
    main()

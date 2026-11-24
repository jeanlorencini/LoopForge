#!/usr/bin/env python3
"""Add the runtime preflight to an exported acceptance-test SOP."""

import argparse
from hashlib import sha256
import json
from pathlib import Path


MANIFEST = "tests/ambiente.json"
MANIFEST_OUTPUT = {
    "mediaType": "application/json",
    "name": "ambiente-aceite",
    "path": MANIFEST,
    "required": True,
}

TEST_PROMPT = (
    '\n\nDeclare os pré-requisitos das duas suítes em tests/ambiente.json, com '
    'as chaves os ("windows", "linux", "darwin" ou "any"), commands (lista de '
    'programas externos no PATH antes dos testes) e env (lista dos nomes de variáveis '
    'que o executor precisa fornecer antes dos testes, sem valores). Liste os requisitos '
    'reais dos scripts e dos casos, inclusive ferramentas invocadas por um auxiliar. '
    'Use "windows" quando os testes precisam executar o aplicativo ou APIs do Windows. '
    'Por exemplo, para Rust com aceitação nativa no Windows, declare os "windows", '
    'commands ["cargo", "rustc", "powershell.exe"] e env ["LOCALAPPDATA"] '
    'quando a suíte usa todos esses recursos. Use listas vazias quando não houver '
    'requisitos daquela espécie. Não substitua testes reais por simulação para '
    'contornar uma incompatibilidade de ambiente. Registre versões e preparação '
    'do executor em testes.md.'
    '\nPara a checagem inicial de I2, execute as suítes somente se o ambiente '
    'já tiver os requisitos declarados. Se faltar algum, confira a sintaxe '
    'dos scripts com sh -n, registre os requisitos e entregue as duas suítes '
    'para a etapa de validação verificar o ambiente.'
)

FRESH_FILES = 'FRESH_FILES = ("testes.md", "verify.sh", "verify-reservado.sh")'
FRESH_FILES_WITH_MANIFEST = FRESH_FILES[:-1] + f', "{MANIFEST}")'
SUCCESS = 'print("I2: entregáveis validados.")'
IMPORTS = "import ast\nimport os\nimport platform\nimport shutil\n"
BASE_VALIDATOR = "b603dc8eda2c753216e1bf17749b85f30f2f79b608b2694ad40351df49d7b2f6"
PREFLIGHT_VALIDATOR = "2030cd2368144e7329a1f1d376b14b248cf1cab5918ed1f112be59f90b612741"
PREFLIGHT = r'''manifest_path = Path("tests/ambiente.json")
try:
    requirements = json.loads(manifest_path.read_text(encoding="utf-8-sig"))
except (OSError, UnicodeError, ValueError):
    fail("tests/ambiente.json precisa conter JSON válido")

if not isinstance(requirements, dict) or set(requirements) != {"os", "commands", "env"}:
    fail("tests/ambiente.json precisa declarar os, commands e env")
if requirements["os"] not in ("any", "linux", "windows", "darwin"):
    fail("os em tests/ambiente.json precisa ser any, linux, windows ou darwin")

for field, pattern in (("commands", r"[A-Za-z0-9_][A-Za-z0-9_.+-]*"), ("env", r"[A-Za-z_][A-Za-z0-9_]*")):
    entries = requirements[field]
    if not isinstance(entries, list) or not all(isinstance(name, str) and re.fullmatch(pattern, name) for name in entries):
        fail(f"{field} em tests/ambiente.json precisa ser uma lista de nomes simples")

problems = []
actual_os = platform.system().lower()
if requirements["os"] not in ("any", actual_os):
    problems.append(f"sistema exigido: {requirements['os']}, executor atual: {actual_os}")
missing_commands = [name for name in requirements["commands"] if not shutil.which(name)]
if missing_commands:
    problems.append("programas fora do PATH: " + ", ".join(missing_commands))
missing_env = [name for name in requirements["env"] if name not in os.environ]
if missing_env:
    problems.append("variáveis de ambiente ausentes: " + ", ".join(missing_env))
if problems:
    raise SystemExit(
        f"{STAGE} bloqueado: ambiente de verificação incompatível ({' | '.join(problems)}). "
        "Escolha um sandbox com esses requisitos antes de aprovar os testes e iniciar I4."
    )
'''


def apply_preflight(workflow):
    nodes = {node["id"]: node for node in workflow["nodes"]}
    tests = nodes["tests"]
    validation = nodes["validate-tests"]
    prompt, outputs, script = tests["taskPrompt"], tests["artifactOutputs"], validation["script"]
    if tests.get("type") != "agent" or validation.get("type") != "script" or validation.get("scriptLanguage") != "python":
        raise ValueError("Expected the Python I2 acceptance-test validator.")
    digest = sha256(script.encode("utf-8")).hexdigest()
    matching = [output for output in outputs if
                output.get("path") == MANIFEST or output.get("name") == MANIFEST_OUTPUT["name"]]
    if digest == PREFLIGHT_VALIDATOR and prompt.endswith(TEST_PROMPT) and matching == [MANIFEST_OUTPUT]:
        return
    if MANIFEST in prompt or matching or MANIFEST in script:
        raise ValueError("The workflow already has a different runtime preflight.")
    if digest != BASE_VALIDATOR:
        raise ValueError("Expected the Python I2 acceptance-test validator.")

    tests["taskPrompt"] = prompt + TEST_PROMPT
    tests["artifactOutputs"] = [*outputs, MANIFEST_OUTPUT]
    validation["script"] = (
        script.replace("import ast\n", IMPORTS, 1)
        .replace(FRESH_FILES, FRESH_FILES_WITH_MANIFEST, 1)
        .replace(SUCCESS, PREFLIGHT + "\n" + SUCCESS, 1)
    )


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("workflow", type=Path, help="exported workflow JSON")
    parser.add_argument("output", type=Path, help="path for the updated workflow JSON")
    args = parser.parse_args()
    try:
        workflow = json.loads(args.workflow.read_text(encoding="utf-8-sig"))
        apply_preflight(workflow)
        args.output.write_text(json.dumps(workflow, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    except (OSError, ValueError, KeyError, TypeError) as exc:
        parser.exit(1, f"{exc}\n")


if __name__ == "__main__":
    main()

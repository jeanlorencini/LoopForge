#!/usr/bin/env python3
"""Run emitted SOP scripts in isolated Git workspaces. Pass original exports to replay."""

from contextlib import suppress
from hashlib import sha256
import json
import os
from pathlib import Path
import platform
import runpy
import signal
import subprocess
import sys
import tempfile
import time
import unittest


HELPER = Path(__file__).with_name("apply-sop-reliability.py")
SOP = runpy.run_path(str(HELPER))
EXPORTS = [Path(arg).resolve() for arg in sys.argv[1:]]
SHELL = str(Path(os.environ.get("ProgramFiles", "C:/Program Files")) / "Git/bin/sh.exe") if os.name == "nt" else "sh"


def write_file(path, contents):
    with path.open("wb") as stream:
        stream.write(contents if isinstance(contents, bytes) else contents.encode("utf-8"))
        stream.flush()
        os.fsync(stream.fileno())


class SOPFixture(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="sop-fixture-")
        self.addCleanup(temporary.cleanup)
        self.temp = Path(temporary.name)
        self.root = self.temp / "workspace with space"
        self.root.mkdir()
        self.repo = self.root / "clone"
        self.repo.mkdir()
        self.git("init", "-q", "-b", "main")
        self.git("config", "user.name", "Synthetic Tester")
        self.git("config", "user.email", "synthetic@example.invalid")
        self.write("clone/product.txt", "original\n")
        self.git("add", "product.txt")
        self.git("commit", "-qm", "original")
        self.gate("spec")
        self.gate("tests")
        self.write("spec.md", "# Synthetic\n- CA001: Ready\n\nReferência a CA001.\n")
        self.write("testes.md", "# Synthetic evidence\n")
        self.write("tests/test_visible.py", "from pathlib import Path\ndef test_CA001_visible():\n    assert Path('spec.md').is_file()\n")
        self.write("tests-reservados/test_hidden.py", "from pathlib import Path\ndef test_CA001_reserved():\n    assert Path('tests-reservados/test_hidden.py').is_file()\n")
        self.write("verify.sh", self.case_script("tests/test_visible.py", "test_CA001_visible"))
        self.write("verify-reservado.sh", self.case_script("tests-reservados/test_hidden.py", "test_CA001_reserved", """
test -f tests-reservados/test_hidden.py || exit 78
if test -f .sop/infrastructure; then exit 127; fi
test ! -f .sop/assertion || exit 1
"""))
        self.manifest()

    @property
    def store(self):
        return self.root / ".sop/workflow-state"

    def case_script(self, file, case, checks=""):
        return f'''# SOP-CASE: {case}
if read answer; then exit 2; fi
{checks}
"$SOP_FIXTURE_PYTHON" - <<'CASE_PY'
import json, os, runpy
from pathlib import Path

mode = os.environ.get("SOP_FIXTURE_RESULT")
if mode != "skip":
    runpy.run_path({file!r})[{case!r}]()
if mode == "zero":
    raise SystemExit(0)
path = Path(os.environ["SOP_SUITE_RECEIPT"])
receipt = dict(nonce=os.environ["SOP_SUITE_NONCE"], cases={{{case!r}: "pass"}})
prior = Path(".sop/prior-receipt.json")
if mode == "stale":
    receipt = json.loads(prior.read_text(encoding="utf-8"))
elif mode == "save":
    with prior.open("w", encoding="utf-8") as file:
        json.dump(receipt, file)
        file.flush()
        os.fsync(file.fileno())
elif mode == "missing":
    receipt["cases"] = {{}}
elif mode == "skip":
    receipt["cases"][{case!r}] = "skip"
with path.open("w", encoding="utf-8") as file:
    json.dump(receipt, file)
    file.flush()
    os.fsync(file.fileno())
CASE_PY
'''

    def write(self, name, text):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        write_file(path, text)
        return path

    def git(self, *command, repo=None):
        return subprocess.run(
            ["git", "-C", str(repo or self.repo), *command], check=True, capture_output=True,
            encoding="utf-8", env={**os.environ, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull},
        ).stdout.strip()

    def init_local_origin(self):
        remote = self.temp / "remote.git"
        remote.mkdir()
        self.git("init", "--bare", "-q", "-b", "main", repo=remote)
        self.git("remote", "add", "origin", str(remote))
        self.git("push", "-q", "origin", "main")
        self.git("remote", "set-head", "origin", "main")

    def gate(self, actor, attempt=1):
        self.write(f".agentflow/runtime/{actor}-{attempt}-1/run-context.json", "{}")
        self.write(f".agentflow/gate-results/{actor}/attempt-{attempt}.json", '{"decision":"pass","reason":"synthetic"}')

    def manifest(self, **options):
        body = dict(os="any", commands=[], env=[], envNonempty=[], inputs=[], interactive=False)
        self.write("tests/ambiente.json", json.dumps(body | options))

    def stage(self, script, expected, message=None, environment=None):
        file = self.temp / "script.sh"
        write_file(file, script)
        env = {**os.environ, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull,
               "SOP_FIXTURE_PYTHON": sys.executable}
        env.update(environment or {})
        syntax = subprocess.run([SHELL, "-n", str(file)], cwd=self.root, env=env, capture_output=True)
        self.assertEqual(syntax.returncode, 0, "generated POSIX shell syntax")
        result = subprocess.run([SHELL, str(file)], cwd=self.root, input=b"available to outer shell\n",
                                capture_output=True, env=env, timeout=60)
        diagnostic = (result.stdout + result.stderr).decode("utf-8", errors="replace")
        self.assertEqual(result.returncode, expected, diagnostic)
        if message:
            self.assertIn(message, diagnostic)
        return diagnostic

    def configure(self, **options):
        self.manifest(**options)
        return SOP["shell_stage"](SOP["PREFLIGHT"], "preflight()")

    def test_runtime_requirements(self):
        self.stage(self.configure(interactive=True), 78, "automatizadas")
        other = "windows" if platform.system() != "Windows" else "linux"
        self.stage(self.configure(os=other), 78, "Sistema exigido")
        if other == "windows":
            self.stage(self.configure(os="windows", commands=["cargo", "rustc"], minimumVersions={"rustc": "1.95"}),
                       78, "Sistema exigido: windows")
        self.stage(self.configure(commands=["sop-missing-test-command"]), 78, "fora do PATH")
        self.stage(self.configure(envNonempty=["SOP_FIXTURE_REQUIRED_VALUE"]), 78, "exigem valor")
        self.stage(self.configure(env=["SOP_FIXTURE_EMPTY"]), 0, environment={"SOP_FIXTURE_EMPTY": ""})
        self.stage(self.configure(inputs=["fixtures/sample.txt"]), 78, "ausente ou vazio")
        self.write("fixtures/sample.txt", "")
        self.stage(self.configure(inputs=["fixtures/sample.txt"]), 78, "ausente ou vazio")
        self.write("fixtures/sample.txt", "synthetic\n")
        self.stage(self.configure(inputs=["fixtures/sample.txt"], commands=["git"], minimumVersions={"git": "99.0"}), 78,
                   "99.0")
        self.stage(self.configure(inputs=["fixtures/sample.txt"], commands=["git"], minimumVersions={"git": "1.0"}), 0)

    def synthetic_scripts(self):
        return {
            "validate-tests": SOP["shell_stage"](
                SOP["PREFLIGHT"] + SOP["CRITERIA"] + SOP["SEAL"],
                'preflight()\nstore()\nrecord(STORE / "validation.json", acceptance(1))'),
            "reserve": SOP["shell_stage"](SOP["CRITERIA"] + SOP["SEAL"], "reserve()"),
            "verify": SOP["shell_stage"](SOP["PREFLIGHT"] + SOP["SEAL"] + SOP["GIT"] + SOP["VERIFY"], "verify(False)"),
            "bind-review": SOP["shell_stage"](SOP["SEAL"] + SOP["GIT"], "bind_review()"),
            "verify-final": SOP["shell_stage"](SOP["PREFLIGHT"] + SOP["SEAL"] + SOP["GIT"] + SOP["VERIFY"], "verify(True)"),
            "git-commit": SOP["shell_stage"](SOP["SEAL"] + SOP["GIT"], 'delivery_gate("before-commit")'),
        }

    def test_validation_freeze_and_verification(self):
        self.exercise_workflow(self.synthetic_scripts())

    def test_verification_requires_a_fresh_result_from_every_declared_case(self):
        scripts = self.synthetic_scripts()
        self.write("spec.md", "- CA001: Ready\n- CA002: Covered\n")
        self.stage(scripts["validate-tests"], 78, "cobrir todos os CAs")
        self.write("tests-reservados/test_hidden.py", "def test_CA002_reserved():\n    assert True\n")
        self.write("verify-reservado.sh", self.case_script("tests-reservados/test_hidden.py", "test_CA002_reserved"))
        self.stage(scripts["validate-tests"], 0)
        self.stage(scripts["reserve"], 0)
        for mode in ("zero", "missing", "skip"):
            with self.subTest(mode=mode):
                self.stage(scripts["verify"], 78, "resultado", {"SOP_FIXTURE_RESULT": mode})
        self.stage(scripts["verify"], 0, environment={"SOP_FIXTURE_RESULT": "save"})
        self.stage(scripts["verify"], 78, "execução atual", {"SOP_FIXTURE_RESULT": "stale"})

    def test_verification_imports_sealed_python_without_changing_inputs(self):
        visible = self.write("tests/fixture_visible.py", "VALUE = 42\n")
        self.write("tests-reservados/fixture_reserved.py", "VALUE = 73\n")
        for script, directory, module, value in (
            ("verify.sh", "tests", "fixture_visible", 42),
            ("verify-reservado.sh", "tests-reservados", "fixture_reserved", 73),
        ):
            path = f"{directory}/test_import.py"
            self.write(path, f'''import sys
sys.path.insert(0, "{directory}")
import {module}
def test_CA001_import():
    assert {module}.VALUE == {value}
''')
            self.write(script, self.case_script(path, "test_CA001_import"))
        scripts = self.synthetic_scripts()
        self.stage(scripts["validate-tests"], 0)
        self.stage(scripts["reserve"], 0)
        environment = dict(SOP_FIXTURE_PYTHON=sys.executable, PYTHONDONTWRITEBYTECODE="", PYTHONPYCACHEPREFIX="")
        self.stage(scripts["verify"], 0, environment=environment)
        self.assertFalse((self.root / "tests/__pycache__").exists())
        write_file(visible, "VALUE = 99\n")
        self.stage(scripts["verify"], 78, "divergiram da aprovação", environment=environment)

    def test_verification_recognizes_a_unique_nested_git_worktree(self):
        self.repo = self.repo.rename(self.temp / "main checkout")
        worktree = self.root / "nested checkout"
        self.git("worktree", "add", "-q", "--detach", str(worktree), "HEAD")
        self.assertTrue((worktree / ".git").is_file())
        self.write("not a checkout/.git", "invalid marker\n")
        scripts = self.synthetic_scripts()
        self.stage(scripts["validate-tests"], 0)
        self.stage(scripts["reserve"], 0)
        self.stage(scripts["verify"], 0)
        self.git("worktree", "add", "-q", "--detach", str(self.root / "second checkout"), "HEAD")
        self.stage(scripts["verify"], 78, "checkout Git identificável")

    def test_verification_rejects_sop_state_in_the_product_index(self):
        scripts = self.synthetic_scripts()
        self.stage(scripts["validate-tests"], 0)
        self.stage(scripts["reserve"], 0)
        self.git("init", "-q", "-b", "main", repo=self.root)
        self.git("add", "-f", ".sop/workflow-state", repo=self.root)
        self.stage(scripts["verify"], 78, "fora do índice do Git")

    @unittest.skipUnless(os.environ.get("SOP_FIXTURE_CHECKPOINT_TOOL"), "requires the runtime checkpoint test")
    def test_checkpoint_recovers_frozen_tests_and_bound_review(self):
        def restore(name):
            target = self.temp / name
            tool = os.environ["SOP_FIXTURE_CHECKPOINT_TOOL"]
            result = subprocess.run(
                [tool, "-test.run=^TestCheckpointRestoresSOPState$"], capture_output=True,
                env={**os.environ, "SOP_FIXTURE_CHECKPOINT_SOURCE": str(self.root),
                     "SOP_FIXTURE_CHECKPOINT_TARGET": str(target)}, timeout=30,
            )
            self.assertEqual(result.returncode, 0, result.stdout.decode("utf-8", errors="replace"))
            self.root, self.repo = target, target / "clone"
            self.assertFalse((target / ".agentflow").exists())

        scripts = self.synthetic_scripts()
        self.manifest(inputs=["fixtures/acceptance.txt"])
        self.write("fixtures/acceptance.txt", "approved\n")
        self.stage(scripts["validate-tests"], 0)
        self.stage(scripts["reserve"], 0)
        self.write(".agentflow/private-home/codex/auth.json", '{"synthetic":true}')
        restore("after reserve")
        self.stage(scripts["reserve"], 0)
        self.stage(scripts["verify"], 0)
        self.gate("review")
        self.write("review.md", f"Reviewed-Git-Tree: {self.git('write-tree')}\n")
        self.stage(scripts["bind-review"], 0)
        restore("after review")
        self.stage(scripts["verify-final"], 0)
        restore("after final verification")
        self.stage(scripts["git-commit"], 0)
        self.write("fixtures/acceptance.txt", "changed\n")
        self.stage(scripts["git-commit"], 78, "insumos declarados divergiram")
        self.write("fixtures/acceptance.txt", "approved\n")
        self.gate("review", 2)
        self.stage(scripts["git-commit"], 78, "revisão mudou")
        self.gate("tests", 2)
        self.stage(scripts["verify"], 78, "Reserva ausente")
        self.write("spec.md", "- CA001: Tampered\n")
        restore("changed inputs")
        self.stage(scripts["verify"], 78, "divergiram da aprovação")

    @unittest.skipUnless(EXPORTS, "requires a local published SOP export")
    def test_delivery_reconciles_a_lost_merge_response(self):
        source = json.loads(EXPORTS[0].read_text(encoding="utf-8-sig"))
        SOP["apply_reliability"](source)
        scripts = {node["id"]: node["script"] for node in source["nodes"] if "script" in node}
        self.init_local_origin()
        self.stage(scripts["git-branch"], 0)
        self.stage(scripts["validate-tests"], 0)
        self.stage(scripts["reserve"], 0)
        self.write("clone/product.txt", "candidate\n")
        self.git("add", "product.txt")
        self.stage(scripts["verify"], 0)
        self.gate("review")
        self.write("review.md", f"Reviewed-Git-Tree: {self.git('write-tree')}\n")
        self.stage(scripts["bind-review"], 0)
        self.stage(scripts["verify-final"], 0)
        self.stage(scripts["git-commit"], 0)
        self.git("remote", "set-url", "origin", "https://github.com/example/sop-fixture.git")
        self.git("config", "credential.helper", "!f() { printf '%s\\n' username=fixture password=fixture; }; f")

        identity = {name: (self.repo / ".sop" / file).read_text().strip() for name, file in {
            "branch": "git-branch", "base": "git-base", "head": "git-approved-head", "base_sha": "git-approved-base",
        }.items()}
        snapshot = self.temp / "provider.json"
        write_file(snapshot, json.dumps(dict(identity, puts=0, merged=False, mergeable="clean")))
        stub = self.temp / "stub"
        stub.mkdir()
        write_file(stub / "sitecustomize.py", PROVIDER_STUB)
        environment = dict(PYTHONPATH=str(stub), SOP_FIXTURE_PROVIDER=str(snapshot), NO_PROXY="",
                           HTTPS_PROXY="http://127.0.0.1:1")
        self.stage('"$SOP_FIXTURE_PYTHON" -c "import sitecustomize; assert sitecustomize.installed"', 0,
                   environment=environment)
        self.stage(scripts["git-merge"], 1, environment=environment)
        merged = json.loads(snapshot.read_text(encoding="utf-8"))
        self.assertTrue(merged["merged"])
        saved, = self.store.glob("merge-*.json")
        self.assertEqual(json.loads(saved.read_text(encoding="utf-8")),
                         dict(identity, repo="example/sop-fixture", number=17))
        self.stage(scripts["git-merge"], 0, environment=environment)
        self.assertEqual(json.loads(snapshot.read_text(encoding="utf-8"))["puts"], 1)
        for drift, message in (
            (dict(merge_base="b" * 40), "duas referências aprovadas"),
            (dict(pr_number=23), "identidade do PR"),
            (dict(merged=False, remote_base="b" * 40), "mudou"),
            (dict(merged=False, mergeable="blocked"), "fixture-ci"),
        ):
            with self.subTest(drift=drift):
                write_file(snapshot, json.dumps(merged | drift))
                self.stage(scripts["git-merge"], 1, message, environment=environment)
                self.assertEqual(json.loads(snapshot.read_text(encoding="utf-8"))["puts"], 1)

    @unittest.skipIf(os.name == "nt", "Windows process termination is covered by exposure recovery")
    def test_posix_termination_cleans_reserved_exposure(self):
        self.write("verify-reservado.sh", "# SOP-CASE: test_CA001_reserved\ntouch .sop/started\nsleep 30\n")
        scripts = self.synthetic_scripts()
        self.stage(scripts["validate-tests"], 0)
        self.stage(scripts["reserve"], 0)
        file = self.temp / "interrupt.sh"
        write_file(file, scripts["verify"])
        with subprocess.Popen([SHELL, str(file)], cwd=self.root, start_new_session=True,
                              env={**os.environ, "SOP_FIXTURE_PYTHON": sys.executable},
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL) as process:
            try:
                deadline = time.monotonic() + 10
                while not (self.root / ".sop/started").exists() and time.monotonic() < deadline:
                    time.sleep(0.05)
                self.assertTrue((self.root / "tests-reservados").is_dir(), "the suite began with reserved exposure")
            finally:
                with suppress(ProcessLookupError):
                    os.killpg(process.pid, signal.SIGTERM)
            process.wait(timeout=10)
        deadline = time.monotonic() + 10
        while (self.store / "exposed.json").exists() and time.monotonic() < deadline:
            time.sleep(0.05)
        self.assertFalse((self.store / "exposed.json").exists())
        self.assertFalse((self.root / "tests-reservados").exists())
        self.assertFalse((self.root / "verify-reservado.sh").exists())

    def exercise_workflow(self, scripts):
        run = lambda stage, expected=0, message=None: self.stage(scripts[stage], expected, message)
        run("verify", 78, "Reserva ausente")
        manifest = self.root / "tests/ambiente.json"
        manifest_bytes = manifest.read_bytes()
        manifest.unlink()
        run("validate-tests", 78, "tests/ambiente.json")
        write_file(manifest, manifest_bytes)
        run("validate-tests")

        protected = self.root / "tests-reservados/test_hidden.py"
        original = protected.read_bytes()
        write_file(protected, original + b"# changed\n")
        run("reserve", 78, "reservados mudaram")
        self.assertTrue(protected.exists())
        write_file(protected, original)
        run("reserve")
        selected = json.loads((self.store / "current-seal.json").read_text(encoding="utf-8"))
        seal = self.store / selected["directory"]
        raw_seal = (seal / "approval.json").read_bytes()
        (self.store / "validation.json").unlink()
        run("reserve")
        self.assertEqual((seal / "approval.json").read_bytes(), raw_seal)
        self.assertFalse(protected.exists())

        self.write("clone/product.txt", "candidate\n")
        self.git("add", "product.txt")
        displaced = (seal / "approval.json").rename(self.store / "displaced.json")
        run("verify", 78, "Reserva ausente")
        displaced.rename(seal / "approval.json")
        failure = self.write(".sop/assertion", "1")
        run("verify", 1, "verify-reservado.sh")
        self.assertFalse((self.root / "tests-reservados").exists())
        self.assertFalse((self.root / "verify-reservado.sh").exists())
        failure.unlink()
        broken = self.write(".sop/infrastructure", "1")
        run("verify", 78, "falha de execução")
        self.assertFalse((self.root / "tests-reservados").exists())
        broken.unlink()

        # Represent a killed process after it recorded ownership and began the copy.
        write_file(self.store / "exposed.json", json.dumps({"seal": sha256(raw_seal).hexdigest()}))
        self.write("tests-reservados/incomplete", "partial\n")
        run("verify")
        self.assertFalse((self.root / "tests-reservados").exists())
        tree = self.git("write-tree")
        self.gate("review")
        self.write("review.md", "Reviewed-Git-Tree: " + "0" * len(tree) + "\n")
        run("bind-review", 78, "árvore Git testada")
        self.write("review.md", f"Reviewed-Git-Tree: {tree}\n")
        run("bind-review")

        self.write("clone/product.txt", "adjusted\n")
        self.git("add", "product.txt")
        self.write(".sop/assertion", "1")
        run("verify-final", 1, "verify-reservado.sh")
        (self.root / ".sop/assertion").unlink()
        self.write(".sop/infrastructure", "1")
        run("verify-final", 78, "falha de execução")
        (self.root / ".sop/infrastructure").unlink()
        run("verify-final", 79, "nova revisão")
        run("git-commit", 78)
        self.gate("review", 2)
        self.write("review.md", f"Reviewed-Git-Tree: {self.git('write-tree')}\n")
        run("bind-review")
        run("verify-final")
        verified_tree = self.git("write-tree")

        self.write("clone/product.txt", "drift\n")
        self.git("add", "product.txt")
        run("git-commit", 78, "mudou depois")
        self.write("clone/product.txt", "adjusted\n")
        self.git("add", "product.txt")
        run("git-commit")
        if "git-merge" in scripts:
            self.assertEqual(self.git("rev-parse", "HEAD^{tree}"), verified_tree)
            self.write("clone/product.txt", "after commit\n")
            self.git("add", "product.txt")
            run("git-merge", 78, "mudou depois")

    @unittest.skipUnless(EXPORTS, "requires a local published SOP export")
    def test_published_export(self):
        for export in EXPORTS:
            out = self.temp / "candidate.json"
            repeat = self.temp / "unchanged.json"
            source = json.loads(export.read_text(encoding="utf-8-sig"))

            def apply(input_path, output_path, expected):
                result = subprocess.run([sys.executable, "-B", str(HELPER), str(input_path), str(output_path)],
                                        capture_output=True)
                self.assertEqual(result.returncode, expected, result.stderr.decode("utf-8", errors="replace"))

            apply(export, out, 0)
            apply(out, repeat, 0)
            self.assertEqual(out.read_bytes(), repeat.read_bytes())
            derived = json.loads(out.read_text(encoding="utf-8"))
            previous = {node["id"]: node for node in source["nodes"]}
            nodes = {node["id"]: node for node in derived["nodes"]}
            for name in ("approve-tests", "approve-merge", "git-branch"):
                self.assertEqual(previous[name], nodes[name])
            for name in ("tests", "fix"):
                self.assertEqual(previous[name]["taskPrompt"], nodes[name]["taskPrompt"])
            self.assertIn("envNonempty", nodes["tests"]["agentInstructions"])
            self.assertEqual(nodes["verify-final"]["rejectTo"], "review")
            self.assertEqual(nodes["verify-final"]["maxReworks"], 1)
            self.assertEqual(nodes["verify-final"]["nonRetryableExitCodes"], [1, 78])

            altered = self.temp / "altered.json"
            invalid = self.temp / "invalid.json"
            inert = next(node for node in source["nodes"] if node["id"] == "validate-tests")
            block = SOP["LEGACY"]["PREFLIGHT"]
            inert["script"] = inert["script"].replace(block, repr(block)) if block in inert["script"] else inert["script"] + "\n" + repr(block)
            write_file(altered, json.dumps(source))
            apply(altered, invalid, 1)
            self.assertFalse(invalid.exists())
            installed = next(node for node in derived["nodes"] if node["id"] == "reserve")
            altered_bytes = installed["script"] + "\n# syntactic but unrecognized edit"
            original_script = installed["script"]
            installed["script"] = altered_bytes
            write_file(altered, json.dumps(derived))
            apply(altered, invalid, 1)
            installed["script"] = original_script

            for node in nodes.values():
                if node.get("scriptLanguage") == "shell":
                    file = self.temp / "check.sh"
                    write_file(file, node["script"])
                    result = subprocess.run([SHELL, "-n", str(file)], cwd=self.root, capture_output=True)
                    self.assertEqual(result.returncode, 0, f"shell syntax at {node['id']}")

        scripts = {name: node["script"] for name, node in nodes.items() if "script" in node}
        self.write("spec.md", "# Missing criteria\n")
        self.stage(scripts["validate-spec"], 1, "critério de aceitação numérico")
        self.write("spec.md", "# Duplicate\n- CA001: first\n- CA-01: duplicate\n")
        self.stage(scripts["validate-spec"], 1, "positivos e únicos")
        self.write("spec.md", "# Valid\n- CA001: first\n\nA referência CA001 é válida.\n")
        self.stage(scripts["validate-spec"], 0)
        runtime_root = self.root / ".agentflow/runtime"
        gate_root = self.root / ".agentflow/gate-results"
        (runtime_root / "spec-1-1").rename(runtime_root / "child-run--spec-1-1")
        (gate_root / "spec").rename(gate_root / "child-run--spec")
        self.stage(scripts["validate-spec"], 1)
        (runtime_root / "child-run--spec-1-1").rename(runtime_root / "spec-1-1")
        (gate_root / "child-run--spec").rename(gate_root / "spec")

        self.init_local_origin()
        self.stage(scripts["git-branch"], 0)
        self.exercise_workflow(scripts)


PROVIDER_STUB = '''
import io
import json
import os
from pathlib import Path
from urllib import error, parse, request


def urlopen(received, *args, **kwargs):
    snapshot = Path(os.environ["SOP_FIXTURE_PROVIDER"])
    state = json.loads(snapshot.read_text(encoding="utf-8"))
    url = parse.urlsplit(received.full_url)
    assert url.netloc == "api.github.com"
    repository = "example/sop-fixture"
    path = parse.unquote(url.path).removeprefix("/repos/" + repository)
    method = received.get_method()
    merge_sha = "a" * 40
    pull = {
        "number": state.get("pr_number", 17), "state": "closed" if state["merged"] else "open", "merged": state["merged"],
        "merge_commit_sha": merge_sha, "mergeable_state": state["mergeable"],
        "head": {"sha": state["head"], "ref": state["branch"], "repo": {"full_name": repository}},
        "base": {"sha": merge_sha if state["merged"] else state["base_sha"], "ref": state["base"],
                 "repo": {"full_name": repository}},
    }
    if method == "GET" and path.startswith("/git/ref/heads/"):
        branch = path.removeprefix("/git/ref/heads/")
        refs = {state["branch"]: state["head"],
                state["base"]: merge_sha if state["merged"] else state.get("remote_base", state["base_sha"])}
        result = {"object": {"sha": refs[branch]}}
    elif method == "GET" and path == "/pulls":
        result = [] if state["merged"] else [pull]
    elif method == "GET" and path == "/pulls/17":
        result = pull
    elif method == "GET" and path == "/git/commits/" + merge_sha:
        result = {"sha": merge_sha, "parents": [{"sha": state.get("merge_base", state["base_sha"])}, {"sha": state["head"]}]}
    elif method == "GET" and path == "/commits/" + state["head"] + "/check-runs":
        result = {"check_runs": [{"name": "fixture-ci", "status": "completed", "conclusion": "failure"}]}
    elif method == "PUT" and path == "/pulls/17/merge":
        assert json.loads(received.data) == {"merge_method": "merge", "sha": state["head"]}
        state.update(puts=state["puts"] + 1, merged=True)
        with snapshot.open("w", encoding="utf-8") as file:
            json.dump(state, file)
            file.flush()
            os.fsync(file.fileno())
        raise error.URLError("synthetic response loss")
    else:
        raise AssertionError((method, path))
    body = io.BytesIO(json.dumps(result).encode())
    body.status = 200
    return body


request.urlopen = urlopen
installed = True
'''


if __name__ == "__main__":
    checkpoint_test = "SOPFixture.test_checkpoint_recovers_frozen_tests_and_bound_review"
    unittest.main(argv=[sys.argv[0]], defaultTest=checkpoint_test if os.environ.get("SOP_FIXTURE_CHECKPOINT_TOOL") else None)

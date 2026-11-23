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


class SOPFixture(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="sop-fixture-")
        self.addCleanup(temporary.cleanup)
        self.temp = Path(temporary.name)
        self.root = self.temp / "workspace with space"
        self.root.mkdir()
        self.repo = self.root / "clone"
        self.repo.mkdir()
        self.store = self.temp / ("." + self.root.name + ".sop-reservado")
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
        self.write("tests/test_visible.py", "def test_CA001_visible():\n    pass\n")
        self.write("tests-reservados/test_hidden.py", "def test_CA001_reserved():\n    pass\n")
        self.write("verify.sh", "if read answer; then exit 2; fi\nexit 0\n")
        self.write("verify-reservado.sh", """if read answer; then exit 2; fi
test -f tests-reservados/test_hidden.py || exit 78
if test -f .sop/infrastructure; then exit 127; fi
test ! -f .sop/assertion
""")
        self.manifest()

    def write(self, name, text):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(text.encode("utf-8"))
        return path

    def git(self, *command, repo=None):
        return subprocess.run(
            ["git", "-C", str(repo or self.repo), *command], check=True, capture_output=True,
            encoding="utf-8", env={**os.environ, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull},
        ).stdout.strip()

    def gate(self, actor, attempt=1):
        self.write(f".agentflow/runtime/{actor}-{attempt}-1/run-context.json", "{}")
        self.write(f".agentflow/gate-results/{actor}/attempt-{attempt}.json", '{"decision":"pass","reason":"synthetic"}')

    def manifest(self, **options):
        body = dict(os="any", commands=[], env=[], envNonempty=[], inputs=[], interactive=False)
        self.write("tests/ambiente.json", json.dumps(body | options))

    def stage(self, script, expected, message=None, environment=None):
        file = self.temp / "script.sh"
        file.write_bytes(script.encode("utf-8"))
        env = {**os.environ, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull}
        env.update(environment or {})
        syntax = subprocess.run([SHELL, "-n", str(file)], cwd=self.root, env=env, capture_output=True)
        self.assertEqual(syntax.returncode, 0, "generated POSIX shell syntax")
        result = subprocess.run([SHELL, str(file)], cwd=self.root, input=b"available to outer shell\n",
                                capture_output=True, env=env, timeout=30)
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
                SOP["PREFLIGHT"] + SOP["SEAL"], 'preflight()\nstore()\nrecord(STORE / "validation.json", acceptance(1))'),
            "reserve": SOP["shell_stage"](SOP["SEAL"], "reserve()"),
            "verify": SOP["shell_stage"](SOP["PREFLIGHT"] + SOP["SEAL"] + SOP["GIT"] + SOP["VERIFY"], "verify(False)"),
            "bind-review": SOP["shell_stage"](SOP["SEAL"] + SOP["GIT"], "bind_review()"),
            "verify-final": SOP["shell_stage"](SOP["PREFLIGHT"] + SOP["SEAL"] + SOP["GIT"] + SOP["VERIFY"], "verify(True)"),
            "git-commit": SOP["shell_stage"](SOP["SEAL"] + SOP["GIT"], 'delivery_gate("before-commit")'),
        }

    def test_validation_freeze_and_verification(self):
        self.exercise_workflow(self.synthetic_scripts())

    @unittest.skipIf(os.name == "nt", "Windows process termination is covered by exposure recovery")
    def test_posix_termination_cleans_reserved_exposure(self):
        self.write("verify-reservado.sh", "touch .sop/started\nsleep 30\n")
        scripts = self.synthetic_scripts()
        self.stage(scripts["validate-tests"], 0)
        self.stage(scripts["reserve"], 0)
        file = self.temp / "interrupt.sh"
        file.write_bytes(scripts["verify"].encode("utf-8"))
        with subprocess.Popen([SHELL, str(file)], cwd=self.root, start_new_session=True,
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
        manifest.write_bytes(manifest_bytes)
        run("validate-tests")

        protected = self.root / "tests-reservados/test_hidden.py"
        original = protected.read_bytes()
        protected.write_bytes(original + b"# changed\n")
        run("reserve", 78, "reservados mudaram")
        self.assertTrue(protected.exists())
        protected.write_bytes(original)
        run("reserve")
        seal = self.store / "seal-1"
        raw_seal = (seal / "approval.json").read_bytes()
        (self.store / "validation.json").unlink()
        run("reserve")
        self.assertEqual((seal / "approval.json").read_bytes(), raw_seal)
        self.assertFalse(protected.exists())

        self.write("clone/product.txt", "candidate\n")
        self.git("add", "product.txt")
        displaced = seal.rename(self.store / "displaced")
        run("verify", 78, "Reserva ausente")
        displaced.rename(seal)
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
        (self.store / "exposed.json").write_text(json.dumps({"seal": sha256(raw_seal).hexdigest()}), encoding="utf-8")
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
            altered.write_text(json.dumps(source), encoding="utf-8")
            apply(altered, invalid, 1)
            self.assertFalse(invalid.exists())
            installed = next(node for node in derived["nodes"] if node["id"] == "reserve")
            altered_bytes = installed["script"] + "\n# syntactic but unrecognized edit"
            original_script = installed["script"]
            installed["script"] = altered_bytes
            altered.write_text(json.dumps(derived), encoding="utf-8")
            apply(altered, invalid, 1)
            installed["script"] = original_script

            for node in nodes.values():
                if node.get("scriptLanguage") == "shell":
                    file = self.temp / "check.sh"
                    file.write_bytes(node["script"].encode("utf-8"))
                    result = subprocess.run([SHELL, "-n", str(file)], cwd=self.root, capture_output=True)
                    self.assertEqual(result.returncode, 0, f"shell syntax at {node['id']}")

        scripts = {name: node["script"] for name, node in nodes.items() if "script" in node}
        self.write("spec.md", "# Missing criteria\n")
        self.stage(scripts["validate-spec"], 1, "critério de aceitação numérico")
        self.write("spec.md", "# Duplicate\n- CA001: first\n- CA-01: duplicate\n")
        self.stage(scripts["validate-spec"], 1, "positivos e únicos")
        self.write("spec.md", "# Valid\n- CA001: first\n\nA referência CA001 é válida.\n")
        self.stage(scripts["validate-spec"], 0)

        remote = self.temp / "remote.git"
        remote.mkdir()
        self.git("init", "--bare", "-q", "-b", "main", repo=remote)
        self.git("remote", "add", "origin", str(remote))
        self.git("push", "-q", "origin", "main")
        self.git("remote", "set-head", "origin", "main")
        self.stage(scripts["git-branch"], 0)
        self.exercise_workflow(scripts)


if __name__ == "__main__":
    unittest.main(argv=[sys.argv[0]])

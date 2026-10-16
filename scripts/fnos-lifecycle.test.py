"""Execute fnOS hooks in an isolated Bash harness with mocked OS/user commands.
This validates hook contracts, not actual fnOS user switching or service networking.
"""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
BASH = os.environ.get("MICAST_TEST_BASH") or shutil.which("bash") or "C:/Tools/Git/bin/bash.exe"
if os.name == "nt" and Path("C:/Tools/Git/bin/bash.exe").exists():
    BASH = "C:/Tools/Git/bin/bash.exe"


def shellpath(path):
    text = Path(path).resolve().as_posix()
    return "/" + text[0].lower() + text[2:] if os.name == "nt" else text


class Lifecycle(unittest.TestCase):
    def setUp(self):
        (ROOT / ".run").mkdir(exist_ok=True)
        self.temp = tempfile.TemporaryDirectory(prefix="lifecycle-", dir=ROOT / ".run")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.app = self.root / "application"
        self.data = self.root / "private data"
        self.fake = self.root / "commands"
        self.hooks = self.root / "hooks"
        for path in [self.app / "bin", self.app / "audio", self.data, self.fake, self.hooks]:
            path.mkdir(parents=True, exist_ok=True)
        self.log = self.root / "calls"
        (self.root / "environment").write_text('export PATH="$HOOK_PATH"\n', encoding="utf-8")
        self.env = os.environ.copy()
        self.env.update(TRIM_APPDEST=shellpath(self.app), TRIM_PKGVAR=shellpath(self.data),
                        TRIM_USERNAME="fixture-user", TRIM_TEMP_LOGFILE=shellpath(self.root / "error"),
                        BASH_ENV=shellpath(self.root / "environment"), HOOK_PATH=shellpath(self.fake) + ":/usr/bin:/bin", HOOK_CALLS=shellpath(self.log), PATH=shellpath(self.fake) + ":/usr/bin:/bin")
        for name in ["chown", "runuser", "curl"]:
            self.script(self.fake / name, 'printf "%s\\n" "' + name + ' $*" >> "$HOOK_CALLS"')
        for name in ["micast-go", "../audio/ffmpeg"]:
            self.script(self.app / "bin" / name, 'printf "%s\\n" "binary $*" >> "$HOOK_CALLS"')
        for source in (ROOT / "packaging/fnos/cmd").iterdir():
            self.script(self.hooks / source.name, source.read_text(encoding="utf-8"), full=True)
        self.script(self.hooks / "main", 'printf "%s\\n" "service $*" >> "$HOOK_CALLS"')

    def script(self, path, body, full=False):
        path.write_text(body if full else "#!/bin/bash\nset -eu\n" + body + "\n", encoding="utf-8", newline="\n")
        path.chmod(0o755)

    def runhook(self, name, expected=0, **variables):
        result = subprocess.run([BASH, shellpath(self.hooks / name)], env=self.env | variables,
                                capture_output=True, text=True, encoding="utf-8", timeout=10)
        self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
        return result

    def seed(self):
        files = ["account.key", "account.enc", "access.key", "admin-password", "config.json"]
        for name in files:
            (self.data / name).write_text("fixture-" + name)
        return {name: (self.data / name).read_bytes() for name in files}

    def test_all_lifecycle_scripts_have_valid_bash_syntax(self):
        for path in (ROOT / "packaging/fnos/cmd").iterdir():
            result = subprocess.run([BASH, "-n", shellpath(path)], capture_output=True, encoding="utf-8", timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_install_without_password_and_reinstall_preserves_credentials(self):
        before = self.seed()
        self.runhook("install_init", wizard_legal_accept="2026-10-16")
        self.runhook("install_callback")
        self.runhook("install_callback")
        self.assertEqual(before, {name: (self.data / name).read_bytes() for name in before})
        self.assertIn("chown -R -H fixture-user", self.log.read_text(encoding="utf-8"))

    def test_install_requires_current_legal_acceptance(self):
        self.runhook("install_init", expected=1, wizard_legal_accept="")
        self.runhook("install_init", expected=1, wizard_legal_accept="old")
        self.runhook("install_init", expected=1, wizard_legal_accept='["old","2026-10-16"]')
        self.runhook("install_init", wizard_legal_accept="2026-10-16")
        self.runhook("install_init", wizard_legal_accept='[ "2026-10-16" ]')
        self.runhook("install_init", wizard_legal_accept='"2026-10-16"')

    def test_upgrade_stops_before_backup_and_removes_obsolete_backup(self):
        before = self.seed()
        backup = self.data / "upgrade-backup"
        backup.mkdir()
        (backup / "advertise-ip").write_text("old")
        self.runhook("upgrade_init")
        self.assertEqual(self.log.read_text(encoding="utf-8").splitlines()[0], "service stop")
        self.assertEqual(before, {name: (backup / name).read_bytes() for name in before})
        self.assertFalse((backup / "advertise-ip").exists())
        self.runhook("upgrade_callback")
        calls = self.log.read_text(encoding="utf-8")
        self.assertIn("--restore-upgrade", calls)
        self.assertIn("chown -R -H fixture-user", calls)

    def test_uninstall_preserves_by_default_and_removes_only_selected_login(self):
        before = self.seed()
        self.runhook("upgrade_init")
        self.runhook("uninstall_init")
        self.runhook("uninstall_callback")
        self.assertEqual(before, {name: (self.data / name).read_bytes() for name in before})
        self.runhook("uninstall_callback", wizard_data_policy="remove_login")
        for name in ["account.key", "account.enc", "access.key", "admin-password"]:
            self.assertFalse((self.data / name).exists())
            self.assertFalse((self.data / "upgrade-backup" / name).exists())
        self.assertTrue((self.data / "config.json").exists())

    def test_uninstall_remove_all_clears_devices_and_upgrade_backup(self):
        self.seed()
        self.runhook("upgrade_init")
        (self.data / "cache").mkdir()
        (self.data / "cache/media").write_text("audio")
        (self.data / "micast-go.log").write_text("log")
        self.runhook("uninstall_callback", wizard_data_policy="remove_all")
        self.assertTrue(self.data.is_dir())
        self.assertEqual(list(self.data.iterdir()), [])
        self.runhook("install_callback")
        self.assertFalse((self.data / "config.json").exists())
        self.assertFalse((self.data / "upgrade-backup").exists())

    def test_uninstall_remove_all_rejects_unsafe_target(self):
        self.seed()
        self.runhook("uninstall_callback", expected=1, wizard_data_policy="remove_all", TRIM_PKGVAR="/")
        self.runhook("uninstall_callback", expected=1, wizard_data_policy="remove_all", TRIM_PKGVAR=shellpath(self.app))
        self.runhook("uninstall_callback", expected=1, wizard_data_policy="unknown")
        self.assertTrue((self.data / "config.json").exists())

    def control(self, body, expected=0):
        source = (ROOT / "packaging/fnos/cmd/main").read_text(encoding="utf-8")
        library = source.rsplit('\nmkdir -p -- "$TRIM_PKGVAR"', 1)[0]
        # Compress waits in function-level simulations; keep production timeouts unchanged.
        library = library.replace("i<200", "i<3").replace("i<20;", "i<2;")
        self.script(self.root / "control", library + "\n" + body, full=True)
        result = subprocess.run([BASH, shellpath(self.root / "control")], env=self.env,
                                capture_output=True, text=True, encoding="utf-8", timeout=10)
        self.assertEqual(result.returncode, expected, result.stdout + result.stderr)

    def test_stop_does_not_signal_foreign_pid(self):
        (self.data / "micast-go.pid").write_text("987654321")
        self.control('''
readlink(){ if [[ "$2" == /proc/* ]]; then echo /unrelated/program; else echo "$BINARY"; fi; }
kill(){ echo "unexpected signal" >&2; return 99; }
healthy(){ return 1; }
stop_service
[ ! -e "$PID_FILE" ]
''')

    def test_live_socket_without_owned_pid_prevents_cleanup(self):
        (self.data / "micast-go.pid").write_text("invalid")
        self.control('''
healthy(){ return 0; }
if stop_service; then exit 90; fi
[ -f "$PID_FILE" ]
''')

    def test_deleted_executable_is_owned_and_stop_is_idempotent(self):
        (self.data / "micast-go.pid").write_text("987654321")
        self.control('''
state="$TRIM_PKGVAR/running"; touch "$state"
readlink(){ if [[ "$2" == /proc/* ]]; then echo "$BINARY (deleted)"; else echo "$BINARY"; fi; }
kill(){ if [ "$1" = -0 ]; then if [ "$2" = 987654321 ]; then [ -f "$state" ]; else builtin kill -0 "$2"; fi; else rm -f "$state"; fi; }
healthy(){ [ -f "$state" ]; }
is_running
stop_service
stop_service
[ ! -e "$PID_FILE" ]
''')

    def test_stop_failure_keeps_pid_for_recovery(self):
        (self.data / "micast-go.pid").write_text("987654321")
        self.control('''
readlink(){ echo "$BINARY"; }
kill(){ if [ "$1" = -0 ]; then return 0; fi; return 1; }
sleep(){ :; }
healthy(){ return 0; }
if stop_service; then exit 90; fi
[ -s "$PID_FILE" ]
''')

    def test_status_without_pid_returns_not_running(self):
        self.script(self.hooks / "main", (ROOT / "packaging/fnos/cmd/main").read_text(encoding="utf-8"), full=True)
        result = subprocess.run([BASH, shellpath(self.hooks / "main"), "status"], env=self.env,
                                capture_output=True, text=True, encoding="utf-8", timeout=10)
        self.assertEqual(result.returncode, 3, result.stderr)

    def test_start_reuses_running_process_and_passes_private_paths(self):
        self.control('''
state="$TRIM_PKGVAR/running"
readlink(){ echo "$BINARY"; }
kill(){ if [ "$1" = -0 ]; then if [ "$2" = 987654321 ]; then [ -f "$state" ]; else builtin kill -0 "$2"; fi; else rm -f "$state"; fi; }
healthy(){ [ -f "$state" ]; }
nohup(){ "$@"; }
setsid(){ "$@"; }
runuser(){ printf "%s\\n" "$*" > "$TRIM_PKGVAR/launch"; echo 987654321 > "$PID_FILE"; touch "$state"; /bin/sleep 0.5; }
start_service
first="$(cat "$TRIM_PKGVAR/launch")"
start_service
[ "$first" = "$(cat "$TRIM_PKGVAR/launch")" ]
[[ "$first" == *"--data-dir $TRIM_PKGVAR"* ]]
[[ "$first" == *"--gateway-socket $SOCKET_FILE"* ]]
stop_service
''')


if __name__ == "__main__":
    unittest.main(verbosity=2)

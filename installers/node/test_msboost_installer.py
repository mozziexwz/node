"""Offline checks of the real embedded installer guards, hello, and rollback."""
import hashlib
import io
import json
import os
import posixpath
from pathlib import Path
import re
import shutil
import socket
import stat
import subprocess
import sys
import tempfile
import threading
import types
import unittest
from unittest import mock

HERE = Path(__file__).resolve().parent
SCRIPT = (HERE / "msboost.sh").read_text(encoding="utf-8")

def heredoc(marker):
    return SCRIPT.split("<<'" + marker + "'\n", 1)[1].split("\n" + marker + "\n", 1)[0] + "\n"

def function(name):
    result = re.search(r"^" + name + r"\(\) \{\n.*?^\}\n", SCRIPT, re.M | re.S)
    if result is None:
        raise AssertionError("missing function " + name)
    return result.group(0)

def bash():
    return shutil.which("bash") or ("C:/Program Files/Git/bin/bash.exe" if Path("C:/Program Files/Git/bin/bash.exe").is_file() else None)

def unit(name):
    if name == "msboost-tcp-probe.service":
        return heredoc("MSBOOST_TCP_PROBE_UNIT").encode()
    text = function("write_main_unit").split('  cat > "$1" <<EOF\n')[1].split("\nEOF\n")[0] + "\n"
    for key, value in {"APP_USER":"msboost", "APP_GROUP":"msboost", "STATE_DIR":"/var/lib/msboost", "BIN":"/usr/local/bin/msboost", "APP_DIR":"/etc/msboost"}.items():
        text = text.replace("${" + key + "}", value)
    return text.encode()

class VirtualLinux:
    def __init__(self, managed=True, probe=True):
        self.files, self.links, self.loaded, self.dropins = {}, {}, {}, {}
        self.uid, self.gid, self.active, self.managed = 123, 124, set(), managed
        for name in ("msboost.service", "msboost-tcp-probe.service"):
            self.put("/stage/" + name, unit(name))
        for file, marker in (("relay_tcp_probe.py", "MSBOOST_TCP_PROBE_PY"), ("tcp-probe.json", "MSBOOST_TCP_PROBE_CONFIG")):
            self.put("/stage/app/" + file, heredoc(marker).encode())
        if managed:
            self.put("/etc/msboost/install.env", self.state(), mode=0o600)
            self.put("/etc/msboost/config.yaml", b"secret credentials", gid=self.gid)
            main = unit("msboost.service")
            if not probe:
                main = main.replace(b"Wants=network-online.target msboost-tcp-probe.service", b"Wants=network-online.target")
            self.put("/etc/systemd/system/msboost.service", main, mode=0o644)
            self.put("/usr/local/bin/msboost", b"engine", mode=0o755)
            self.put("/var/lib/msboost/ruleset/msboost-filter.mrs", b"rules", uid=self.uid, gid=self.gid)
            if probe:
                for file in ("relay_tcp_probe.py", "tcp-probe.json"):
                    self.put("/etc/msboost/" + file, self.files["/stage/app/" + file][0], gid=self.gid)
                self.put("/etc/systemd/system/msboost-tcp-probe.service", unit("msboost-tcp-probe.service"), mode=0o644)

    def state(self):
        return ("MANAGED_BY=msboost-installer-v1\nMAIN_UNIT_SHA256=" + hashlib.sha256(unit("msboost.service")).hexdigest() + "\nTCP_PROBE_UNIT_SHA256=" + hashlib.sha256(unit("msboost-tcp-probe.service")).hexdigest() + "\n").encode()

    def put(self, path, data, uid=0, gid=0, mode=0o640, nlink=1):
        self.files[path] = (data, uid, gid, mode, nlink)

    def isdir(self, path):
        return path not in self.links and any(key.startswith(path.rstrip("/") + "/") for key in list(self.files) + list(self.links))

    def exists(self, path):
        return path in self.files or path in self.links or self.isdir(path)

    def lstat(self, path):
        if path in self.links:
            return types.SimpleNamespace(st_mode=stat.S_IFLNK | 0o777, st_uid=0, st_gid=0, st_nlink=1)
        if path in self.files:
            data, uid, gid, mode, nlink = self.files[path]
            return types.SimpleNamespace(st_mode=stat.S_IFREG | mode, st_uid=uid, st_gid=gid, st_nlink=nlink)
        if self.isdir(path):
            return types.SimpleNamespace(st_mode=stat.S_IFDIR | 0o750, st_uid=0, st_gid=self.gid, st_nlink=2)
        raise FileNotFoundError(path)

    def listdir(self, path):
        return sorted({key[len(path)+1:].split("/",1)[0] for key in list(self.files)+list(self.links) if key.startswith(path+"/")})

    def walk(self, root, followlinks=False):
        for directory in sorted({os.path.dirname(key) for key in list(self.files)+list(self.links) if key.startswith(root+"/")}):
            yield directory, [], [os.path.basename(key) for key in list(self.files)+list(self.links) if os.path.dirname(key)==directory]

    def output(self, args, **kwargs):
        service, key = args[2], args[4]
        if key == "DropInPaths":
            return self.dropins.get(service, "")
        path = "/etc/systemd/system/" + service
        if key == "LoadState":
            return "loaded" if self.exists(path) or service in self.active else "not-found"
        return self.loaded.get(service, path if self.exists(path) else "")

    def check(self):
        pwd = types.SimpleNamespace(getpwnam=lambda name: types.SimpleNamespace(pw_uid=self.uid,pw_gid=self.gid,pw_dir="/var/lib/msboost",pw_shell="/usr/sbin/nologin"))
        grp = types.SimpleNamespace(getgrnam=lambda name: types.SimpleNamespace(gr_gid=self.gid))
        with mock.patch.dict(sys.modules,{"pwd":pwd,"grp":grp}), mock.patch.object(sys,"argv",["guard","/stage","1" if self.managed else "0"]), mock.patch("builtins.open",side_effect=lambda path,*a,**kw:io.BytesIO(self.files[path][0])), mock.patch("os.path.lexists",side_effect=self.exists), mock.patch("os.path.islink",side_effect=lambda path:path in self.links), mock.patch("os.path.isdir",side_effect=self.isdir), mock.patch("os.path.isfile",side_effect=lambda path:path in self.files), mock.patch("os.path.ismount",return_value=False), mock.patch("os.path.abspath",side_effect=posixpath.abspath), mock.patch("os.lstat",side_effect=self.lstat), mock.patch("os.listdir",side_effect=self.listdir), mock.patch("os.walk",side_effect=self.walk), mock.patch("os.readlink",side_effect=lambda path:self.links[path]), mock.patch("subprocess.check_output",side_effect=self.output), mock.patch("subprocess.call",side_effect=lambda args:0 if args[-1] in self.active else 3):
            exec(compile(heredoc("MSBOOST_IDENTITY_PY"),"embedded-guard","exec"),{})

class InstallerTests(unittest.TestCase):
    def test_packaging_and_syntax(self):
        self.assertEqual(heredoc("MSBOOST_TCP_PROBE_PY").encode(),(HERE/"tcp_probe.py").read_bytes())
        compile(heredoc("MSBOOST_TCP_PROBE_PY"),"probe","exec")
        if bash():
            result=subprocess.run([bash(),"-n",str(HERE/"msboost.sh")],capture_output=True)
            self.assertEqual(result.returncode,0,result.stderr)
        with tempfile.TemporaryDirectory() as directory:
            config=Path(directory)/"config.json"
            config.write_text(heredoc("MSBOOST_TCP_PROBE_CONFIG"),encoding="utf-8")
            result=subprocess.run([sys.executable,"-B",str(HERE/"tcp_probe.py"),"--config",str(config),"--validate-config"],capture_output=True)
            self.assertEqual(result.returncode,0,result.stderr)
        self.assertIn('probe_hello "${SELFTEST_PORT}"',SCRIPT)
        self.assertNotIn("${PROBE_PORT}",function("configure_firewall"))
        self.assertIn('(( candidate != PROBE_PORT )) || continue',function("random_free_port"))
        self.assertIn('(( PORT != PROBE_PORT )) || fail',SCRIPT)

    def test_fresh_legacy_and_complete_v2(self):
        VirtualLinux(managed=False).check()
        VirtualLinux(probe=False).check()
        VirtualLinux().check()

    def test_unknown_and_unsafe_resources(self):
        mutations=[
            lambda x:x.put("/etc/msboost/unowned",b"third party"),
            lambda x:x.put("/var/lib/msboost/unowned",b"third party"),
            lambda x:x.put("/etc/msboost/relay_tcp_probe.py",b"third party",gid=x.gid),
            lambda x:x.put("/etc/msboost/tcp-probe.json",b"{}",gid=x.gid),
            lambda x:x.put("/etc/systemd/system/msboost-tcp-probe.service",unit("msboost-tcp-probe.service")+b"ExecStartPost=/bin/true\n",mode=0o644),
            lambda x:x.files.pop("/etc/msboost/tcp-probe.json"),
            lambda x:x.links.update({"/etc/msboost/relay_tcp_probe.py":"/third-party"}),
            lambda x:x.put("/etc/msboost/relay_tcp_probe.py",heredoc("MSBOOST_TCP_PROBE_PY").encode(),gid=x.gid,nlink=2),
            lambda x:x.put("/etc/systemd/system/msboost-tcp-probe.service.d/override.conf",b"override"),
            lambda x:x.dropins.update({"msboost.service":"/etc/systemd/system/service.d/override.conf"}),
            lambda x:x.loaded.update({"msboost.service":"/usr/lib/systemd/system/msboost.service"}),
            lambda x:x.links.update({"/run/systemd/system/msboost.service":"/third-party"}),
            lambda x:x.links.update({"/etc/systemd/system/unknown.target.wants/msboost.service":"/etc/systemd/system/msboost.service"}),
            lambda x:x.links.update({"/etc/systemd/system/multi-user.target.wants":"/third-party"}),
            lambda x:x.links.update({"/etc/systemd/system/third-party.service":"/etc/systemd/system/msboost.service"}),
            lambda x:x.put("/etc/systemd/system/multi-user.target.wants",b"unexpected-file"),
            lambda x:setattr(x,"uid",0),
            lambda x:setattr(x,"gid",0),
        ]
        for index,mutate in enumerate(mutations):
            with self.subTest(index=index):
                fixture=VirtualLinux();mutate(fixture)
                with self.assertRaises(SystemExit):fixture.check()

    def test_unknown_active_transient_unit(self):
        fixture=VirtualLinux(managed=False)
        fixture.active.add("msboost.service")
        with self.assertRaises(SystemExit):fixture.check()

    def test_masks_require_exact_provenance(self):
        for scope in ("/etc/systemd/system","/run/systemd/system"):
            fixture=VirtualLinux()
            for service,target in (("msboost.service","multi-user.target"),("msboost-tcp-probe.service","msboost.service")):
                if scope.startswith("/etc"):
                    fixture.files.pop(scope+"/"+service)
                fixture.links[scope+"/"+service]="/dev/null"
                fixture.loaded[service]="/dev/null"
                for root in ("/etc/systemd/system","/run/systemd/system"):
                    fixture.links[root+"/"+target+".wants/"+service]="/etc/systemd/system/"+service
            fixture.check()
            if scope.startswith("/etc"):
                fixture.put("/etc/msboost/install.env",b"MANAGED_BY=msboost-installer-v1\n",mode=0o600)
                with self.assertRaises(SystemExit):fixture.check()

    def test_rollback_restores_files_activity_and_all_link_states(self):
        if not bash():self.skipTest("Bash unavailable")
        selected=("rollback_problem","rollback_firewall","rollback_install","unit_state_paths","snapshot_unit_links","clear_unit_links","restore_unit_links","verify_restored_unit_state")
        code="\n".join(function(name) for name in selected)
        # The fixture rewrites all managed paths into an isolated temporary tree.
        # systemctl and owner-only install options are replaced by inert models.
        for active in (0,1):
            for mask in ("none","runtime","persistent"):
                for enabled in ("disabled","persistent","runtime","both"):
                    with self.subTest(active=active,mask=mask,enabled=enabled), tempfile.TemporaryDirectory(dir=HERE) as directory:
                        self.assertIn(HERE, Path(directory).resolve().parents)
                        root=Path(os.path.relpath(directory, Path.cwd())).as_posix()
                        body=ROLLBACK_FIXTURE.replace("@ROOT@",root).replace("@ACTIVE@",str(active)).replace("@MASK@",mask).replace("@ENABLED@",enabled)
                        rewritten=code.replace("/etc/systemd/system",root+"/etc/systemd/system").replace("/run/systemd/system",root+"/run/systemd/system")
                        result=subprocess.run([bash(),"-s"],input=rewritten+"\n"+body,text=True,encoding="utf-8",errors="replace",capture_output=True,timeout=15,env=dict(os.environ, MSYS="winsymlinks:sys") if os.name == "nt" else None)
                        self.assertEqual(result.returncode,0,result.stdout+result.stderr)
                        self.assertIn("ROLLBACK_VERIFIED",result.stdout)

ROLLBACK_FIXTURE=r'''
export PATH=/usr/bin:/bin:$PATH
set -eu
ROOT='@ROOT@'
SERVICE=msboost.service
PROBE_SERVICE=msboost-tcp-probe.service
BIN="$ROOT/bin"
APP_DIR="$ROOT/app"
STATE_DIR="$ROOT/state"
UNIT="$ROOT/etc/systemd/system/$SERVICE"
PROBE_UNIT="$ROOT/etc/systemd/system/$PROBE_SERVICE"
CLIENT_CONFIG="$ROOT/client.json"
BACKUP_DIR="$ROOT/backup"
STAGE_DIR="$ROOT/stage"
ROLLBACK_FAILED=0
CREATED_USER=0
CREATED_GROUP=0
OLD_BIN=1
OLD_APP=1
OLD_STATE=1
OLD_UNIT=1
OLD_PROBE_UNIT=1
OLD_CLIENT=1
OLD_ACTIVE=@ACTIVE@
OLD_PROBE_ACTIVE=@ACTIVE@
FW_UFW_ADDED=0
FW_FIREWALLD_RUNTIME_ADDED=0
FW_FIREWALLD_PERMANENT_ADDED=0
OLD_FW_UFW_REMOVED=0
OLD_FW_FIREWALLD_RUNTIME_REMOVED=0
OLD_FW_FIREWALLD_PERMANENT_REMOVED=0
OLD_FW_FIREWALLD_PERMANENT_REMOVED_OFFLINE=0
warn() { printf '%s\n' "$*" >&2; }
install() {
  local -a args=()
  local dirs=0
  while (( $# )); do
    case "$1" in -o|-g|-m) shift 2 ;; -d) dirs=1; shift ;; *) args+=("$1"); shift ;; esac
  done
  if (( dirs )); then mkdir -p "${args[@]}"; else /usr/bin/cp -P "${args[@]}"; fi
}
cp() {
  local -a args=()
  while (( $# )); do
    case "$1" in -a) args+=(-P -r); shift ;; *) args+=("$1"); shift ;; esac
  done
  /usr/bin/cp "${args[@]}"
}

systemctl() {
  local action=$1 name=${@: -1} target
  case "$action" in
    daemon-reload) return 0 ;;
    stop) rm -f "$ROOT/active-$name"; [[ "$name" != "$SERVICE" ]] || rm -f "$ROOT/active-$PROBE_SERVICE"; return 0 ;;
    start)
      [[ ! -L "$ROOT/etc/systemd/system/$name" && ! -L "$ROOT/run/systemd/system/$name" ]] || return 9
      touch "$ROOT/active-$name"; return 0 ;;
    is-active) [[ -f "$ROOT/active-$name" ]]; return ;;
    is-enabled)
      [[ "$name" == "$SERVICE" ]] && target=multi-user.target || target=msboost.service
      if [[ -L "$ROOT/etc/systemd/system/$name" ]]; then echo masked
      elif [[ -L "$ROOT/run/systemd/system/$name" ]]; then echo masked-runtime
      elif [[ -L "$ROOT/etc/systemd/system/$target.wants/$name" ]]; then echo enabled
      elif [[ -L "$ROOT/run/systemd/system/$target.wants/$name" ]]; then echo enabled-runtime
      else echo disabled; fi
      return 0 ;;
    *) return 99 ;;
  esac
}
mkdir -p "$APP_DIR" "$STATE_DIR" "$STAGE_DIR" "$BACKUP_DIR" "$ROOT/etc/systemd/system" "$ROOT/run/systemd/system"
printf 'original-secret\n' > "$APP_DIR/install.env"
printf 'original-source\n' > "$APP_DIR/relay_tcp_probe.py"
printf 'original-config\n' > "$APP_DIR/tcp-probe.json"
printf 'original-state\n' > "$STATE_DIR/cache.db"
printf 'original-engine\n' > "$BIN"
printf 'original-client\n' > "$CLIENT_CONFIG"
printf 'verified-main-template\n' > "$UNIT"
printf 'verified-probe-template\n' > "$PROBE_UNIT"
cp "$UNIT" "$STAGE_DIR/msboost.service"
cp "$PROBE_UNIT" "$STAGE_DIR/msboost-tcp-probe.service"
for name in "$SERVICE" "$PROBE_SERVICE"; do
  [[ "$name" == "$SERVICE" ]] && target=multi-user.target || target=msboost.service
  for scope in etc run; do
    mkdir -p "$ROOT/$scope/systemd/system/$target.wants"
    if [[ '@ENABLED@' == both || '@ENABLED@' == persistent && "$scope" == etc || '@ENABLED@' == runtime && "$scope" == run ]]; then
      ln -s "$ROOT/etc/systemd/system/$name" "$ROOT/$scope/systemd/system/$target.wants/$name"
    fi
  done
  if [[ '@MASK@' == runtime ]]; then ln -s /dev/null "$ROOT/run/systemd/system/$name"; fi
  if [[ '@MASK@' == persistent ]]; then rm "$ROOT/etc/systemd/system/$name"; ln -s /dev/null "$ROOT/etc/systemd/system/$name"; fi
  [[ '@ACTIVE@' == 0 ]] || touch "$ROOT/active-$name"
  snapshot_unit_links "$name"
done
OLD_ENABLE_STATE=$(systemctl is-enabled "$SERVICE")
OLD_PROBE_ENABLE_STATE=$(systemctl is-enabled "$PROBE_SERVICE")
cp -a "$BIN" "$BACKUP_DIR/bin"
cp -a "$APP_DIR" "$BACKUP_DIR/app"
cp -a "$STATE_DIR" "$BACKUP_DIR/state"
cp -a "$UNIT" "$BACKUP_DIR/unit"
cp -a "$PROBE_UNIT" "$BACKUP_DIR/probe-unit"
cp -a "$CLIENT_CONFIG" "$BACKUP_DIR/client.json"
clear_unit_links "$SERVICE"
clear_unit_links "$PROBE_SERVICE"
rm -f "$UNIT" "$PROBE_UNIT"
printf 'failed-main\n' > "$UNIT"
printf 'failed-probe\n' > "$PROBE_UNIT"
printf 'failed-credentials\n' > "$APP_DIR/install.env"
printf 'failed-source\n' > "$APP_DIR/relay_tcp_probe.py"
printf 'failed-config\n' > "$APP_DIR/tcp-probe.json"
printf 'failed-engine\n' > "$BIN"
printf 'failed-client\n' > "$CLIENT_CONFIG"
touch "$ROOT/active-$SERVICE" "$ROOT/active-$PROBE_SERVICE"
rollback_install
[[ "$ROLLBACK_FAILED" == 0 ]]
[[ "$(cat "$APP_DIR/install.env")" == original-secret ]]
[[ "$(cat "$APP_DIR/relay_tcp_probe.py")" == original-source ]]
[[ "$(cat "$APP_DIR/tcp-probe.json")" == original-config ]]
[[ "$(cat "$BIN")" == original-engine ]]
[[ "$(cat "$CLIENT_CONFIG")" == original-client ]]
for name in "$SERVICE" "$PROBE_SERVICE"; do
  index=0
  while IFS= read -r path; do
    saved="$BACKUP_DIR/$name.links/$index"
    if [[ -L "$saved" ]]; then [[ -L "$path" && "$(readlink "$path")" == "$(readlink "$saved")" ]] || exit 70
    else [[ ! -e "$path" && ! -L "$path" ]] || exit 71; fi
    index=$((index+1))
  done < <(unit_state_paths "$name")
  if [[ '@MASK@' == persistent ]]; then [[ -L "$ROOT/etc/systemd/system/$name" && "$(readlink "$ROOT/etc/systemd/system/$name")" == /dev/null ]] || exit 72; fi
  if [[ '@ACTIVE@' == 1 ]]; then [[ -f "$ROOT/active-$name" ]] || exit 73
  else [[ ! -f "$ROOT/active-$name" ]] || exit 74; fi
done
echo ROLLBACK_VERIFIED
'''

PINNED_ENGINE = {
    "mihomo-linux-amd64-compatible-v1.19.32.gz": "ba3ce607747a07f948fc35780e108a4a7c7f552a38b9bd4d115f313ebcb89c20",
    "mihomo-linux-amd64-v1-v1.19.32.gz": "306f81e723e60ce6b828899a6fe83e1d00e9ecefb2dc8d4d849312a5bc00efdc",
    "mihomo-linux-amd64-v1.19.32.gz": "8451100836c9eda194331c2babfad490b2faf30cebc1a04b0e76fd8ac2d35d10",
    "mihomo-linux-arm64-v1.19.32.gz": "9dd862e28b46ff7d775f169cceebc28deccaa0a9e804237d421cd2571e0caba0",
}
# LOW_ENTROPY_MODE_* enum values from mieru pkg/appctl/proto/base.proto.
LOW_ENTROPY_ENUM = {"32": 1, "40": 2, "48": 3, "56": 4}
LEGACY_CLIENT = """{
  "profiles": [
    {
      "profileName": "msboost",
      "user": {
        "name": "node-user",
        "password": "node-password-1234"
      },
      "servers": [
        {
          "ipAddress": "8.8.8.8",
          "domainName": "",
          "portBindings": [
            {
              "port": 23456,
              "protocol": "TCP"
            }
          ]
        }
      ],
      "mtu": 1400
    }
  ],
  "activeProfile": "msboost",
  "rpcPort": 8964,
  "socks5Port": 10086,
  "loggingLevel": "INFO"
}
"""

def heredoc_function(name):
    # These functions end in a heredoc whose JSON body may start a line with "}".
    start = SCRIPT.index("\n" + name + "() {\n") + 1
    end = SCRIPT.index("\nEOF\n}\n", start) + len("\nEOF\n}\n")
    return SCRIPT[start:end]

def engine_selector():
    body = function("download_engine")
    return body.split("<<'PY'\n", 1)[1].split("\nPY\n", 1)[0] + "\n"

def run_bash(code, env=None):
    result = subprocess.run([bash(), "-s"], input="set -Eeuo pipefail\n" + code, text=True, encoding="utf-8",
                            errors="replace", capture_output=True, timeout=15, env=env)
    return result

def guard_block():
    result = re.search(r'^if \[\[ -n "\$\{MSBOOST_LOW_ENTROPY\+set\}" \]\].*?^fi\n', SCRIPT, re.M | re.S)
    if result is None:
        raise AssertionError("missing explicit low-entropy guard")
    return result.group(0)

class EngineAndTrafficPatternTests(unittest.TestCase):
    def test_pinned_engine_version_files_and_hashes(self):
        self.assertIn('readonly PINNED_ENGINE_VERSION="v1.19.32"\n', SCRIPT)
        self.assertIn('readonly VERSION="${MSBOOST_ENGINE_VERSION:-${PINNED_ENGINE_VERSION}}"\n', SCRIPT)
        rows = [line.split() for line in heredoc("MSBOOST_PINNED_ENGINE").splitlines()]
        self.assertEqual({name: digest for name, digest in rows}, PINNED_ENGINE)
        for name, digest in rows:
            self.assertRegex(digest, r"\A[0-9a-f]{64}\Z")
            self.assertTrue(name.endswith("-v1.19.32.gz"), name)
        # Every name the selector can choose for the pinned version is reviewed.
        selector = function("download_engine")
        for name in PINNED_ENGINE:
            family = "amd64" if "amd64" in name else "arm64"
            self.assertIn("mihomo-linux-" + family, selector)
        self.assertIn('fail "No reviewed SHA-256 is recorded for ${asset_name}."', selector)
        self.assertLess(selector.index('"${calculated,,}" == "${pinned_digest}"'), selector.index('"${calculated,,}" == "${asset_digest,,}"'))
        self.assertIn("only GitHub's release digest will be checked", selector)

    def test_pinned_selection_ignores_unreviewed_assets(self):
        code = engine_selector()
        assets = [{"name": "mihomo-linux-arm64-v8-v1.19.32.gz", "browser_download_url": "https://example.invalid/v8", "digest": "sha256:" + "0" * 64},
                  {"name": "mihomo-linux-arm64-v1.19.32.gz", "browser_download_url": "https://example.invalid/arm64", "digest": "sha256:" + "1" * 64},
                  {"name": "mihomo-linux-amd64-v1.19.32.gz", "browser_download_url": "https://example.invalid/amd64", "digest": "sha256:" + "2" * 64}]
        with tempfile.TemporaryDirectory() as directory:
            release = Path(directory) / "release.json"
            release.write_text(json.dumps({"assets": assets}), encoding="utf-8")
            pinned = " ".join(PINNED_ENGINE)
            out = subprocess.run([sys.executable, "-c", code, str(release), "arm64", "v1.19.32", pinned], capture_output=True, text=True)
            self.assertEqual(out.stdout.splitlines()[1], "mihomo-linux-arm64-v1.19.32.gz")
            out = subprocess.run([sys.executable, "-c", code, str(release), "arm64", "v1.19.32", ""], capture_output=True, text=True)
            self.assertEqual(out.stdout.splitlines()[1], "mihomo-linux-arm64-v8-v1.19.32.gz")
            out = subprocess.run([sys.executable, "-c", code, str(release), "amd64", "v1.19.32", pinned], capture_output=True, text=True)
            self.assertEqual(out.stdout.splitlines()[1], "mihomo-linux-amd64-v1.19.32.gz")

    def test_low_entropy_patterns_are_mieru_protobuf(self):
        if not bash(): self.skipTest("Bash unavailable")
        import base64
        code = function("low_entropy_pattern")
        for level, value in LOW_ENTROPY_ENUM.items():
            with self.subTest(level=level):
                result = run_bash(code + 'low_entropy_pattern ' + level + '\n')
                self.assertEqual(result.returncode, 0, result.stderr)
                # TrafficPattern field 6 (lowEntropy) -> LowEntropyPattern field 1 (mode).
                self.assertEqual(base64.b64decode(result.stdout, validate=True), bytes((0x32, 0x02, 0x08, value)))
        self.assertEqual(run_bash(code + 'low_entropy_pattern off\n').stdout, "")
        self.assertNotEqual(run_bash(code + 'low_entropy_pattern 64\n').returncode, 0)

    def test_explicit_value_is_validated_before_any_change(self):
        if not bash(): self.skipTest("Bash unavailable")
        guard = guard_block()
        start = SCRIPT.index(guard)
        for later in ("\npreflight_checks \"$@\"\n", "\nbootstrap_existing_clock\n", "\ninstall_dependencies\n", "\nconfigure_time\n"):
            self.assertLess(start, SCRIPT.index(later), later)
        code = function("valid_low_entropy") + 'fail() { printf "REJECTED\\n"; exit 1; }\n' + guard + 'printf "ACCEPTED\\n"\n'
        for value in ("off", "56", "48", "40", "32"):
            result = run_bash(code, env=dict(os.environ, MSBOOST_LOW_ENTROPY=value))
            self.assertIn("ACCEPTED", result.stdout, value)
        for value in ("", "on", "OFF", "64", "48 ", " 48", "0", "true", "LOW_ENTROPY_MODE_48"):
            with self.subTest(value=value):
                result = run_bash(code, env=dict(os.environ, MSBOOST_LOW_ENTROPY=value))
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("REJECTED", result.stdout)
        result = run_bash(code, env={key: item for key, item in os.environ.items() if key != "MSBOOST_LOW_ENTROPY"})
        self.assertIn("ACCEPTED", result.stdout)

    def test_default_levels(self):
        if not bash(): self.skipTest("Bash unavailable")
        code = "".join(function(name) for name in ("valid_low_entropy", "low_entropy_pattern", "state_value", "resolve_low_entropy"))
        code += 'fail() { printf "FAILED\\n"; exit 1; }\n'
        cases = [
            # (explicit, force_fresh, managed, recorded, expected)
            (None, "0", 0, None, "48"),          # first installation
            (None, "1", 1, "off", "48"),         # panel deploy / forced fresh credentials
            (None, "0", 1, None, "off"),         # rerun of an installation from before the switch
            (None, "0", 1, "40", "40"),          # rerun keeps the recorded level
            (None, "0", 1, "off", "off"),
            ("off", "1", 0, None, "off"),        # explicit value wins
            ("56", "0", 1, "48", "56"),
            ("32", "0", 1, None, "32"),
        ]
        with tempfile.TemporaryDirectory(dir=HERE) as directory:
            state = Path(directory) / "install.env"
            root = Path(os.path.relpath(directory, Path.cwd())).as_posix()
            for explicit, fresh, managed, recorded, expected in cases:
                with self.subTest(explicit=explicit, fresh=fresh, managed=managed, recorded=recorded):
                    if recorded is None:
                        state.write_text("MANAGED_BY=msboost-installer-v1\nUSER_NAME=old\n", encoding="utf-8")
                    else:
                        state.write_text("MANAGED_BY=msboost-installer-v1\nLOW_ENTROPY=" + recorded + "\n", encoding="utf-8")
                    if not managed:
                        state.unlink()
                    env = {key: item for key, item in os.environ.items() if key not in ("MSBOOST_LOW_ENTROPY", "MSBOOST_FORCE_FRESH")}
                    env["MSBOOST_FORCE_FRESH"] = fresh
                    if explicit is not None:
                        env["MSBOOST_LOW_ENTROPY"] = explicit
                    body = "STATE_FILE='" + root + "/install.env'\nMANAGED_INSTALL=" + str(managed) + "\n" + code
                    body += 'resolve_low_entropy\nprintf "%s|%s\\n" "$LOW_ENTROPY" "$TRAFFIC_PATTERN"\n'
                    result = run_bash(body, env=env)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    level, pattern = result.stdout.strip().split("|")
                    self.assertEqual(level, expected)
                    self.assertEqual(pattern, run_bash(function("low_entropy_pattern") + "low_entropy_pattern " + expected + "\n").stdout)
            state.write_text("MANAGED_BY=msboost-installer-v1\nLOW_ENTROPY=64\n", encoding="utf-8")
            env = {key: item for key, item in os.environ.items() if key not in ("MSBOOST_LOW_ENTROPY", "MSBOOST_FORCE_FRESH")}
            result = run_bash("STATE_FILE='" + root + "/install.env'\nMANAGED_INSTALL=1\n" + code + "resolve_low_entropy\n", env=env)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("FAILED", result.stdout)

    def render(self, level):
        functions = function("low_entropy_pattern") + "".join(heredoc_function(name) for name in ("write_server_config", "write_client_config", "write_selftest_config"))
        with tempfile.TemporaryDirectory(dir=HERE) as directory:
            root = Path(os.path.relpath(directory, Path.cwd())).as_posix()
            body = ("PORT=23456\nUSER_NAME=node-user\nUSER_PASS=node-password-1234\nPUBLIC_IP=8.8.8.8\nSELFTEST_PORT=34567\n"
                    "RULE_URL=https://example.invalid/gfw.mrs\nLOW_ENTROPY=" + level + "\n" + functions +
                    'TRAFFIC_PATTERN="$(low_entropy_pattern "$LOW_ENTROPY")"\n'
                    "write_server_config '" + root + "/server.yaml' /direct.yaml /filter.mrs\n"
                    "write_client_config '" + root + "/client.json'\n"
                    "write_selftest_config '" + root + "/selftest.yaml'\n")
            result = run_bash(body)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            return tuple((Path(directory) / name).read_text(encoding="utf-8") for name in ("server.yaml", "client.json", "selftest.yaml"))

    def test_generated_configurations_per_level(self):
        if not bash(): self.skipTest("Bash unavailable")
        server, client, selftest = self.render("off")
        self.assertEqual(client, LEGACY_CLIENT)
        self.assertNotIn("traffic-pattern", server + selftest)
        self.assertIn("    transport: TCP\n    users:\n", server)
        self.assertIn("    handshake-mode: HANDSHAKE_STANDARD\nrules:\n", selftest)
        import base64
        for level, value in LOW_ENTROPY_ENUM.items():
            with self.subTest(level=level):
                encoded = base64.b64encode(bytes((0x32, 0x02, 0x08, value))).decode()
                server, client, selftest = self.render(level)
                self.assertIn('    transport: TCP\n    traffic-pattern: "' + encoded + '"\n    users:\n', server)
                self.assertEqual(server.count("traffic-pattern"), 1)
                self.assertIn('    handshake-mode: HANDSHAKE_STANDARD\n    traffic-pattern: "' + encoded + '"\nrules:\n', selftest)
                document = json.loads(client)
                profile = document["profiles"][0]
                self.assertEqual(profile["trafficPattern"], {"lowEntropy": {"mode": "LOW_ENTROPY_MODE_" + level}})
                self.assertEqual(list(profile), ["profileName", "user", "servers", "mtu", "trafficPattern"])
                legacy = json.loads(LEGACY_CLIENT)
                del profile["trafficPattern"]
                self.assertEqual(document, legacy)

    def test_install_state_records_level_and_guard_accepts_it(self):
        if not bash(): self.skipTest("Bash unavailable")
        with tempfile.TemporaryDirectory(dir=HERE) as directory:
            root = Path(os.path.relpath(directory, Path.cwd())).as_posix()
            Path(directory, "msboost.service").write_bytes(unit("msboost.service"))
            Path(directory, "msboost-tcp-probe.service").write_bytes(unit("msboost-tcp-probe.service"))
            body = ("MANAGED_MARKER=msboost-installer-v1\nUSER_NAME=u\nUSER_PASS=p\nPORT=1\nVERSION=v1.19.32\nLOW_ENTROPY=48\n"
                    "NEW_FW_UFW_OWNED=0\nNEW_FW_FIREWALLD_RUNTIME_OWNED=0\nNEW_FW_FIREWALLD_PERMANENT_OWNED=0\nFW_FIREWALLD_ZONE=\n"
                    "STAGE_DIR='" + root + "'\n" + heredoc_function("write_install_state") + "write_install_state '" + root + "/install.env'\n")
            result = run_bash(body)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            lines = Path(directory, "install.env").read_text(encoding="utf-8").splitlines()
            self.assertIn("LOW_ENTROPY=48", lines)
            self.assertIn("ENGINE_VERSION=v1.19.32", lines)
        fixture = VirtualLinux()
        data, uid, gid, mode, nlink = fixture.files["/etc/msboost/install.env"]
        fixture.put("/etc/msboost/install.env", data + b"LOW_ENTROPY=48\n", uid, gid, mode, nlink)
        fixture.check()

class HelloTests(unittest.TestCase):
    def run_hello(self, mutate=lambda raw:raw, auth=0, atyp=1, delay=0):
        import time
        listener=socket.socket();listener.bind(("127.0.0.1",0));listener.listen(1)
        port=listener.getsockname()[1]
        errors=[]
        def exact(stream,n):
            data=b""
            while len(data)<n:
                part=stream.recv(n-len(data))
                if not part:raise EOFError()
                data+=part
            return data
        def serve():
            try:
                with listener.accept()[0] as stream:
                    stream.settimeout(2);exact(stream,3);stream.sendall(bytes((5,auth)))
                    if auth:return
                    request=exact(stream,10)
                    if request!=b'\x05\x01\x00\x01\x7f\x00\x00\x01'+(20424).to_bytes(2,"big"):raise AssertionError("wrong remote destination")
                    time.sleep(delay)
                    reply=bytes((5,0,0,atyp))+{1:b'\x00'*4,3:b'\x03abc',4:b'\x00'*16}[atyp]+b'\x00\x00'
                    for byte in reply:stream.sendall(bytes((byte,)))
                    line=b""
                    while not line.endswith(b'\n'):line+=exact(stream,1)
                    response=dict(json.loads(line),node_id='a'*32,success=True)
                    time.sleep(delay)
                    stream.sendall(mutate(json.dumps(response,separators=(",",":")).encode()+b'\n'))
            except (OSError,EOFError):pass
            except Exception as error:errors.append(error)
            finally:listener.close()
        thread=threading.Thread(target=serve,daemon=True);thread.start()
        code=heredoc("MSBOOST_PROBE_HELLO_PY")
        if delay:code=code.replace("time.monotonic() + 5","time.monotonic() + 0.18")
        result=subprocess.run([sys.executable,"-c",code,str(port)],capture_output=True,timeout=7)
        thread.join(3)
        if errors:raise errors[0]
        return result

    def test_fragmented_socks_bound_address_formats(self):
        for atyp in (1,3,4):
            with self.subTest(atyp=atyp):
                result=self.run_hello(atyp=atyp);self.assertEqual(result.returncode,0,result.stderr)

    def test_strict_hello_fields_and_line_boundaries(self):
        mutations=[
            lambda raw:raw.replace(b'"version":2',b'"version":2.0'),
            lambda raw:raw.replace(b'"version":2',b'"version":true'),
            lambda raw:raw.replace(b'"version":2',b'"version":2e0'),
            lambda raw:raw.replace(b'"version":2',b'"version":2,"version":2'),
            lambda raw:raw.replace(b'"success":true',b'"success":false'),
            lambda raw:raw.replace(b'"hello"',b'"connect"'),
            lambda raw:raw.replace(b'"node_id":"',b'"node_id":"\\u0061'),
            lambda raw:raw.replace(b'}',b',"connect_ms":1}'),
            lambda raw:b'\xef\xbb\xbf'+raw,
            lambda raw:raw+b'extra\n',
            lambda raw:b' '*4097+raw,
            lambda raw:raw[:-1],
            lambda raw:raw.replace(b'"node_id":"',b'"node_id":"\x00'),
        ]
        for index,mutate in enumerate(mutations):
            with self.subTest(index=index):self.assertNotEqual(self.run_hello(mutate).returncode,0)

    def test_auth_and_shared_deadline(self):
        self.assertNotEqual(self.run_hello(auth=2).returncode,0)
        self.assertNotEqual(self.run_hello(delay=0.12).returncode,0)

if __name__=="__main__":unittest.main()

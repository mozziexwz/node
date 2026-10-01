"""Opt-in live tests of the actual BAT functions using an owned isolated core.

Use --check for syntax-only checks without reading credentials or using network.
Use --run only after the explicitly authorized node's v2 service has been deployed.
For transparent forwarding, --node must match the entry IP in the supplied
configuration; --expected-exit supplies the final node IP for actual menu checks.
The supplied configuration controls credentials and the entry port; this harness
never rewrites their values or infers which final node executes target connects. Reports
contain results only; core configuration, credentials and raw logs never print.
"""

import argparse
import ast
import base64
import copy
import csv
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import sys
import tempfile
import time


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = Path(__file__).with_name("test-msboost-fullpath.ps1")
LAUNCHER = None
CORE = None
MODES = ("HANDSHAKE_DEFAULT", "HANDSHAKE_STANDARD", "HANDSHAKE_NO_WAIT")
NODE = None
EXPECTED_EXIT = None
# Fixed conservative policy, independent of Python is_global classification.
DENIED_IPV4 = (
    (0x00000000, 8), (0x0a000000, 8), (0x64400000, 10),
    (0x7f000000, 8), (0xa9fe0000, 16), (0xac100000, 12),
    (0xc0000000, 24), (0xc0000200, 24), (0xc0586300, 24),
    (0xc0a80000, 16), (0xc6120000, 15), (0xc6336400, 24),
    (0xcb007100, 24), (0xe0000000, 4), (0xf0000000, 4))
POWERSHELL = Path(os.environ.get("SystemRoot", r"C:\Windows")) / "System32/WindowsPowerShell/v1.0/powershell.exe"
HIDDEN = getattr(subprocess, "CREATE_NO_WINDOW", 0)
SAFE_MESSAGES = frozenset((
    "PowerShell syntax check failed", "Could not snapshot existing core process IDs",
    "Could not resolve the local user SID", "The local user SID was invalid",
    "Could not make the temporary configuration directory private",
    "Isolated describe config failed", "Core configuration isolation was not verified",
    "The isolated listener or handshake mode was not verified",
    "The isolated listener was not owned by the created core process",
    "The owned core exited during startup", "The isolated local proxy did not become ready",
    "PowerShell real-function validation failed", "The PowerShell result did not match its isolated invocation",
))


def public_ipv4(value):
    try:
        address = ipaddress.IPv4Address(value)
    except (ValueError, ipaddress.AddressValueError):
        raise argparse.ArgumentTypeError("Use a canonical public unicast IPv4 address")
    if str(address) != value or any(int(address) >> (32 - prefix) == network >> (32 - prefix)
                                   for network, prefix in DENIED_IPV4):
        raise argparse.ArgumentTypeError("Use a canonical public unicast IPv4 address")
    return value


def public_target(value):
    address, separator, port = value.rpartition(":")
    if not separator or re.fullmatch(r"[1-9][0-9]{0,4}", port) is None or not 1 <= int(port) <= 65535:
        raise argparse.ArgumentTypeError("Use canonical public IPv4:port, with port 1-65535")
    return public_ipv4(address), int(port)


class HarnessError(RuntimeError):
    def __init__(self, message, diagnostic=None):
        super().__init__(message)
        self.diagnostic = diagnostic


def run_private(arguments, **kwargs):
    return subprocess.run(arguments, capture_output=True, creationflags=HIDDEN,
                          timeout=kwargs.pop("timeout", 15), **kwargs)


def run_powershell_code(code, **kwargs):
    encoded = base64.b64encode(code.encode("utf-16-le")).decode("ascii")
    kwargs["env"] = windows_powershell_env(kwargs.get("env"))
    return run_private([str(POWERSHELL), "-NoLogo", "-NoProfile", "-NonInteractive",
                        "-ExecutionPolicy", "Bypass", "-EncodedCommand", encoded], **kwargs)


def isolated_core_env(config_path):
    env = {key: value for key, value in os.environ.items()
           if not key.casefold().startswith("mieru_")}
    env["MIERU_CONFIG_JSON_FILE"] = str(config_path)
    return env


def windows_powershell_env(source=None):
    # PowerShell 7's module path breaks Windows PowerShell 5.1 module autoloading.
    # Let the child construct its own standard module paths instead.
    return {key: value for key, value in (os.environ if source is None else source).items()
            if key.casefold() != "psmodulepath"}


def check_syntax():
    ast.parse(Path(__file__).read_text(encoding="utf-8-sig"))
    env = os.environ.copy()
    env["FULLPATH_CHECK_PS"] = str(SCRIPT)
    env["FULLPATH_CHECK_BAT"] = str(LAUNCHER)
    code = r'''
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
foreach ($path in @($env:FULLPATH_CHECK_PS, $env:FULLPATH_CHECK_BAT)) {
    $source = [IO.File]::ReadAllText($path, [Text.Encoding]::UTF8)
    if ([IO.Path]::GetExtension($path) -eq '.bat') {
        $marker = '#<MSBOOST_PS>'
        $offset = $source.IndexOf($marker, [StringComparison]::Ordinal)
        if ($offset -lt 0) { exit 2 }
        $source = $source.Substring($offset + $marker.Length)
    }
    $tokens = $null; $errors = $null
    $null = [Management.Automation.Language.Parser]::ParseInput($source, [ref]$tokens, [ref]$errors)
    if (@($errors).Count -ne 0) { exit 3 }
}
'''
    checked = run_powershell_code(code, env=env)
    if checked.returncode != 0:
        raise RuntimeError("PowerShell syntax check failed")


def process_ids():
    checked = run_powershell_code("$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue'; ConvertTo-Json -InputObject @(Get-Process -Name MSBOOST -ErrorAction SilentlyContinue | Sort-Object Id | Select-Object -ExpandProperty Id) -Compress")
    if checked.returncode != 0:
        raise RuntimeError("Could not snapshot existing core process IDs")
    return json.loads(checked.stdout.decode("utf-8-sig"))


def file_snapshot(paths):
    return {str(path): hashlib.sha256(path.read_bytes()).hexdigest() if path.exists() else None
            for path in paths}


def active_profile(config):
    matches = [profile for profile in config.get("profiles", [])
               if profile.get("profileName") == config.get("activeProfile")]
    if len(matches) != 1:
        raise ValueError("The source configuration has no unique active profile")
    return matches[0]


def validate_authorized_node(config):
    servers = active_profile(config).get("servers", [])
    if (len(servers) != 1 or servers[0].get("ipAddress") != NODE or
            servers[0].get("domainName")):
        raise ValueError("The source configuration does not match the explicitly authorized entry node")


def secure_private_directory(path):
    """Remove inherited access before writing credentials to the temporary folder."""
    identity = run_private(["whoami.exe", "/user", "/fo", "csv", "/nh"])
    if identity.returncode != 0:
        raise RuntimeError("Could not resolve the local user SID")
    rows = list(csv.reader(identity.stdout.decode(errors="replace").splitlines()))
    if not rows or len(rows[0]) < 2 or re.fullmatch(r"S-1-(?:[0-9]+-)*[0-9]+", rows[0][1]) is None:
        raise RuntimeError("The local user SID was invalid")
    sid = rows[0][1]
    secured = run_private(["icacls.exe", str(path), "/inheritance:r", "/grant:r",
                           "*" + sid + ":(OI)(CI)F", "/grant:r", "*S-1-5-18:(OI)(CI)F"])
    if secured.returncode != 0:
        raise RuntimeError("Could not make the temporary configuration directory private")


def describe_isolated(config_path, local_port, mode):
    env = isolated_core_env(config_path)
    described = run_private([str(CORE), "describe", "config"], env=env, cwd=config_path.parent)
    if described.returncode != 0:
        raise RuntimeError("Isolated describe config failed")
    text = (described.stdout + described.stderr).decode("utf-8-sig")
    parsed = json.loads(text[text.index("{"):text.rindex("}") + 1])
    if parsed.get("socks5Port") != local_port or parsed.get("rpcPort", 0) != 0:
        raise RuntimeError("Core configuration isolation was not verified")
    if (parsed.get("socks5ListenLAN", False) or parsed.get("httpProxyPort", 0) != 0 or
            parsed.get("httpProxyListenLAN", False) or active_profile(parsed).get("handshakeMode") != mode):
        raise RuntimeError("The isolated listener or handshake mode was not verified")
    validate_authorized_node(parsed)
    return env


def wait_for_owned_core(process, port):
    deadline = time.monotonic() + 8.0
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError("The owned core exited during startup")
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.2) as connection:
                connection.settimeout(0.3)
                connection.sendall(b"\x05\x01\x00")
                response = bytearray()
                while len(response) < 2:
                    chunk = connection.recv(2 - len(response))
                    if not chunk:
                        raise OSError("Incomplete local proxy greeting")
                    response.extend(chunk)
                if response == b"\x05\x00" and process.poll() is None:
                    return
        except OSError:
            pass
        time.sleep(0.05)
    raise TimeoutError("The isolated local proxy did not become ready")


def verify_owned_listener(process, port):
    env = os.environ.copy()
    env["MSBOOST_TEST_OWNED_PID"] = str(process.pid)
    env["MSBOOST_TEST_OWNED_PORT"] = str(port)
    code = r'''
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$listeners = @(Get-NetTCPConnection -State Listen -ErrorAction Stop | Where-Object {
    $_.LocalPort -eq [int]$env:MSBOOST_TEST_OWNED_PORT -and $_.LocalAddress -ceq '127.0.0.1'
})
if ($listeners.Count -ne 1 -or $listeners[0].OwningProcess -ne [int]$env:MSBOOST_TEST_OWNED_PID) { exit 4 }
'''
    checked = run_powershell_code(code, env=env)
    if checked.returncode != 0 or process.poll() is not None:
        raise RuntimeError("The isolated listener was not owned by the created core process")


def terminate_owned_core(process):
    if process is None:
        return
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


def invoke_real_functions(env, mode, local_port, target, include_menu):
    address, port = target
    command = [str(POWERSHELL), "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
               "-File", str(SCRIPT), "-Socks5Port", str(local_port), "-GameAddress", address,
               "-GamePort", str(port), "-ExpectedMode", mode, "-ExpectedNode", NODE,
               "-ExpectedExit", EXPECTED_EXIT,
               "-LauncherPath", str(LAUNCHER), "-CorePath", str(CORE)]
    if include_menu:
        command.append("-IncludeMenu")
    checked = run_private(command, env=windows_powershell_env(env), cwd=Path(env["MIERU_CONFIG_JSON_FILE"]).parent,
                          timeout=55 if include_menu else 30)
    if checked.returncode != 0:
        diagnostic = {"exit_code": checked.returncode}
        try:
            failure = json.loads(checked.stdout.decode("utf-8-sig"))
            if isinstance(failure, dict) and failure.get("HarnessFailure") is True:
                for field in ("Stage", "SafeMessage", "ErrorType", "ScriptLine"):
                    value = failure.get(field)
                    if isinstance(value, (str, int)) and len(str(value)) <= 256:
                        diagnostic[field] = value
        except (ValueError, UnicodeError):
            diagnostic["structured_error_available"] = False
        raise HarnessError("PowerShell real-function validation failed", diagnostic)
    report = json.loads(checked.stdout.decode("utf-8-sig"))
    if (not isinstance(report, dict) or report.get("Mode") != mode or
            report.get("Node") != NODE or report.get("ExpectedExit") != EXPECTED_EXIT or
            report.get("IsolatedSocksPort") != local_port):
        raise RuntimeError("The PowerShell result did not match its isolated invocation")
    return report


def main():
    global LAUNCHER, CORE, NODE, EXPECTED_EXIT
    if hasattr(sys.stdout, "reconfigure"):
        sys.stdout.reconfigure(encoding="utf-8")
    parser = argparse.ArgumentParser(description=__doc__)
    action = parser.add_mutually_exclusive_group(required=True)
    action.add_argument("--check", action="store_true", help="Syntax checks only; no core, credentials or network")
    action.add_argument("--run", action="store_true", help="Run authorized live checks after v2 deployment")
    parser.add_argument("--config", type=Path, help="Explicit newly fetched private client JSON; required for --run")
    parser.add_argument("--node", type=public_ipv4, help="Explicit authorized entry IPv4; required for --run, must match client config")
    parser.add_argument("--expected-exit", type=public_ipv4, help="Expected actual menu exit IPv4; defaults to --node, set final node for transparent forwarding")
    parser.add_argument("--launcher", type=Path, required=True, help="Actual full-path BAT, read-only")
    parser.add_argument("--core", type=Path, required=True, help="Existing MSBOOST.exe, never replace or stop existing instances")
    parser.add_argument("--menu", action="store_true", help="Also run actual Check-IpAddress once for the first selected target in the first mode")
    parser.add_argument("--modes", nargs="+", choices=MODES, default=list(MODES))
    targets = parser.add_mutually_exclusive_group()
    targets.add_argument("--targets", nargs="+", choices=("primary", "secondary"), help="Named targets; default primary and secondary")
    targets.add_argument("--target", type=public_target, action="append", help="Repeatable explicit public IPv4:port, replaces named defaults")
    args = parser.parse_args()
    LAUNCHER = args.launcher.resolve()
    CORE = args.core.resolve()
    NODE = args.node
    EXPECTED_EXIT = args.expected_exit or NODE
    if not LAUNCHER.is_file() or not CORE.is_file():
        parser.error("--launcher and --core must name existing files")
    if args.run and (args.config is None or NODE is None):
        parser.error("--run requires explicit --config and --node")
    check_syntax()
    if args.check:
        print(json.dumps({"syntax_checked": True, "network_used": False, "core_started": False}))
        return 0
    if os.name != "nt":
        raise RuntimeError("The isolated real-core runner requires Windows")
    config_source = args.config.resolve()
    if not config_source.is_file():
        raise ValueError("Explicit source configuration does not exist")
    original = json.loads(config_source.read_text(encoding="utf-8-sig"))
    validate_authorized_node(original)
    if original.get("socks5Authentication"):
        raise ValueError("This test requires the existing unauthenticated local SOCKS configuration")
    if args.target:
        selected_targets = [("target_" + str(index + 1), target)
                            for index, target in enumerate(args.target)]
    else:
        named_targets = args.targets or ["primary", "secondary"]
        target_map = {"secondary": ("51.222.56.192", 8484)}
        if "primary" in named_targets:
            source = LAUNCHER.read_text(encoding="utf-8-sig")
            primary_address = re.search(r"(?m)^\$script:GameAddress = '([^']+)'", source).group(1)
            primary_port = re.search(r"(?m)^\$script:GamePort = (\d+)", source).group(1)
            target_map["primary"] = public_target(primary_address + ":" + primary_port)
        selected_targets = [(name, target_map[name]) for name in named_targets]
    watched_files = [config_source, LAUNCHER, CORE] + [LAUNCHER.parent / name for name in
        ("msboost-state.dat", "msboost-current-config.txt", "msboost-last-error.log")]
    before_files = file_snapshot(watched_files)
    before_processes = process_ids()
    results = []
    temporary = tempfile.TemporaryDirectory(prefix="msboost-fullpath-")
    private_root = Path(temporary.name).resolve()
    expected_parent = Path(tempfile.gettempdir()).resolve()
    if private_root.parent != expected_parent or not private_root.name.startswith("msboost-fullpath-"):
        raise RuntimeError("The temporary directory is not within the intended temp root")
    try:
        secure_private_directory(private_root)
        for mode_index, mode in enumerate(args.modes):
            process = None
            isolated_path = private_root / (mode + ".json")
            mode_report = {"node": NODE, "expected_exit": EXPECTED_EXIT, "mode": mode, "measurements": []}
            try:
                mode_report["stage"] = "prepare_isolated_config"
                with socket.socket() as reservation:
                    reservation.bind(("127.0.0.1", 0))
                    local_port = reservation.getsockname()[1]
                config = copy.deepcopy(original)
                config["profiles"] = [copy.deepcopy(active_profile(original))]
                active_profile(config)["handshakeMode"] = mode
                config["socks5Port"] = local_port
                config["socks5ListenLAN"] = False
                config["rpcPort"] = 0
                config["loggingLevel"] = "ERROR"
                config.pop("httpProxyPort", None)
                config.pop("httpProxyListenLAN", None)
                config["advancedSettings"] = dict(config.get("advancedSettings") or {})
                config["advancedSettings"]["noCheckUpdate"] = True
                isolated_path.write_text(json.dumps(config), encoding="utf-8")
                mode_report["stage"] = "describe_isolated_config"
                env = describe_isolated(isolated_path, local_port, mode)
                mode_report["stage"] = "start_owned_core"
                process = subprocess.Popen([str(CORE), "run"], env=env, cwd=private_root,
                                           stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                           stderr=subprocess.DEVNULL, creationflags=HIDDEN)
                mode_report.update({"config_isolation_verified": True, "rpc_disabled": True,
                                    "isolated_core_pid": process.pid, "isolated_socks_port": local_port})
                wait_for_owned_core(process, local_port)
                verify_owned_listener(process, local_port)
                mode_report["owned_listener_verified"] = True
                for target_index, (name, target) in enumerate(selected_targets):
                    mode_report["stage"] = "invoke_" + name
                    include_menu = bool(args.menu and mode_index == 0 and target_index == 0)
                    report = invoke_real_functions(env, mode, local_port, target, include_menu)
                    mode_report["measurements"].append(report)
                mode_report["stage"] = "completed"
            except Exception as error:
                # Do not print exception payloads: native errors can contain config.
                mode_report["error_type"] = type(error).__name__
                if str(error) in SAFE_MESSAGES:
                    mode_report["safe_error_message"] = str(error)
                if isinstance(error, HarnessError) and error.diagnostic is not None:
                    mode_report["diagnostic"] = error.diagnostic
            finally:
                terminate_owned_core(process)
                mode_report["owned_core_stopped"] = process is None or process.poll() is not None
                if isolated_path.exists():
                    isolated_path.unlink()
            results.append(mode_report)
            print(json.dumps(mode_report, ensure_ascii=False), flush=True)
    finally:
        # This is the exact directory created above, checked before recursive cleanup.
        if private_root.parent != expected_parent or not private_root.name.startswith("msboost-fullpath-"):
            raise RuntimeError("Refusing cleanup outside the owned temporary directory")
        temporary.cleanup()
        files_unchanged = file_snapshot(watched_files) == before_files
        processes_unchanged = process_ids() == before_processes
        summary = {"summary": True, "node": NODE, "expected_exit": EXPECTED_EXIT, "source_and_state_unchanged": files_unchanged,
                   "existing_core_processes_unchanged": processes_unchanged,
                   "temporary_config_removed": not private_root.exists()}
        summary["passed"] = bool(files_unchanged and processes_unchanged and
                                 len(results) == len(args.modes) and all(
                                     "error_type" not in result and result["owned_core_stopped"] and
                                     len(result["measurements"]) == len(selected_targets) and
                                     all(item.get("Passed") for item in result["measurements"])
                                     for result in results))
        print(json.dumps(summary), flush=True)
    return 0 if summary["passed"] else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as error:
        print(json.dumps({"fatal_error_type": type(error).__name__, "details_suppressed": True}), flush=True)
        sys.exit(1)

"""Optional actual launcher compatibility checks, using only local TCP fixtures.

Run on Windows with MSBOOST_TEST_LAUNCHER_BAT pointing to the full-path BAT.
PowerShell parses its embedded source and imports top-level function definitions
only. Launcher initialization, menus, core process management and state IO never
run. This forwarding fixture is not a real Mieru or transparent relay path.
"""

import base64
import errno
import importlib.util
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import threading
import time
import unittest


SPEC = importlib.util.spec_from_file_location(
    "msboost_probe", Path(__file__).with_name("tcp_probe.py"))
probe = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(probe)
PUBLIC_TARGET = "35.155.204.207"
LAUNCHER_BAT = os.environ.get("MSBOOST_TEST_LAUNCHER_BAT", "")
POWERSHELL = shutil.which("powershell.exe") if os.name == "nt" else None

PS_FUNCTION_TEST = r'''
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Console]::OutputEncoding = New-Object Text.UTF8Encoding($false)
$encoding = New-Object Text.UTF8Encoding($false, $true)
$source = [IO.File]::ReadAllText($env:MSBOOST_TEST_LAUNCHER_BAT, $encoding)
$marker = '#' + '<' + 'MSBOOST_PS' + '>'
$offset = $source.IndexOf($marker, [StringComparison]::Ordinal)
if ($offset -lt 0) { throw 'Embedded PowerShell marker missing' }
$tokens = $null
$parseErrors = $null
$ast = [Management.Automation.Language.Parser]::ParseInput(
    $source.Substring($offset + $marker.Length), [ref]$tokens, [ref]$parseErrors)
if (@($parseErrors).Count -ne 0) { throw 'Embedded PowerShell has syntax errors' }
$definitions = @($ast.EndBlock.Statements | Where-Object {
    $_ -is [Management.Automation.Language.FunctionDefinitionAst]
})
if ($definitions.Count -eq 0) { throw 'No top-level launcher function definitions' }
foreach ($definition in $definitions) {
    . ([ScriptBlock]::Create($definition.Extent.Text))
}
$script:FullPathProbePort = 20424
$measurement = [Diagnostics.Stopwatch]::StartNew()
$stats = Invoke-FullPathTcpSamples -Socks5Port ([int]$env:MSBOOST_TEST_SOCKS_PORT) `
    -GameAddress $env:MSBOOST_TEST_GAME_ADDRESS -GamePort 8585 `
    -Count 3 -TimeoutMs 2000 -WarmupTimeoutMs 5000
$measurement.Stop()
[pscustomobject]@{
    Stats = $stats
    InvocationMs = $measurement.Elapsed.TotalMilliseconds
    PowerShellVersion = $PSVersionTable.PSVersion.ToString()
    FunctionCount = $definitions.Count
} | ConvertTo-Json -Compress -Depth 8
'''


def read_exact(stream, length):
    chunks = bytearray()
    while len(chunks) < length:
        chunk = stream.recv(length - len(chunks))
        if not chunk:
            raise EOFError("fixture peer closed during SOCKS negotiation")
        chunks.extend(chunk)
    return bytes(chunks)


def fragmented_send(stream, data, fragment_size=1):
    for offset in range(0, len(data), fragment_size):
        stream.sendall(data[offset:offset + fragment_size])
        if offset + fragment_size < len(data):
            time.sleep(0.001)


class LocalPathFixture:
    """SOCKS -> ephemeral real probe -> mapped real loopback target only."""

    def __init__(self, reply_form="ipv4", hello_delay=0, forwarding_delay=0,
                 connect_delay=0, outcome="success"):
        self.reply_form = reply_form
        self.hello_delay = hello_delay
        self.forwarding_delay = forwarding_delay
        self.connect_delay = connect_delay
        self.outcome = outcome
        self.stop_event = threading.Event()
        self.lock = threading.Lock()
        self.session_count = 0
        self.hello_count = 0
        self.connect_calls = []
        self.destinations = []
        self.response_codes = []
        self.target_closes = []
        self.errors = []
        self.clients = set()
        self.workers = []
        self.target = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.target.bind(("127.0.0.1", 0))
        self.target.listen(4)
        self.target.settimeout(0.10)
        self.target_thread = threading.Thread(target=self._accept_target, daemon=True)
        self.target_thread.start()
        self.server = probe.ProbeServer(listen_port=0, connector=self._connect_target)
        self.server_thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.server_thread.start()
        self.socks = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.socks.bind(("127.0.0.1", 0))
        self.socks.listen(4)
        self.socks.settimeout(0.10)
        self.socks_port = self.socks.getsockname()[1]
        self.socks_thread = threading.Thread(target=self._accept_socks, daemon=True)
        self.socks_thread.start()

    def _accept_target(self):
        while not self.stop_event.is_set():
            try:
                client, _ = self.target.accept()
            except socket.timeout:
                continue
            except OSError:
                break
            with client:
                client.settimeout(2)
                try:
                    # The target provides no game handshake or other bytes.
                    closed = client.recv(1) == b""
                    with self.lock:
                        self.target_closes.append(closed)
                except OSError as error:
                    with self.lock:
                        self.errors.append(str(error))

    def _connect_target(self, address, port, timeout):
        with self.lock:
            self.connect_calls.append((address, port, timeout))
        if (address, port) != (PUBLIC_TARGET, 8585):
            raise AssertionError("test connector received unexpected target")
        if self.outcome == "refused":
            # Controlled valid failure, independent of Windows SYN retry timing.
            raise OSError(errno.ECONNREFUSED, "local fixture injected refusal")
        time.sleep(self.connect_delay)
        # Only this test connector maps a declared public address to loopback.
        # Production tcp_connect always connects the requested public address.
        probe.tcp_connect("127.0.0.1", self.target.getsockname()[1], timeout)

    def _accept_socks(self):
        while not self.stop_event.is_set():
            try:
                client, _ = self.socks.accept()
            except socket.timeout:
                continue
            except OSError:
                break
            with self.lock:
                self.session_count += 1
                self.clients.add(client)
            worker = threading.Thread(target=self._forward_session, args=(client,), daemon=True)
            self.workers.append(worker)
            worker.start()

    def _connect_reply(self):
        port = bytes((0, 1))
        if self.reply_form == "ipv4":
            return b"\x05\x00\x00\x01\x7f\x00\x00\x01" + port
        if self.reply_form == "ipv6":
            return b"\x05\x00\x00\x04" + b"\x00" * 15 + b"\x01" + port
        name = b"probe.local"
        return b"\x05\x00\x00\x03" + bytes((len(name),)) + name + port

    def _forward_session(self, client):
        upstream = None
        try:
            client.settimeout(8)
            client.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
            if read_exact(client, 3) != b"\x05\x01\x00":
                raise AssertionError("launcher sent unexpected greeting")
            fragmented_send(client, b"\x05\x00")
            head = read_exact(client, 4)
            if head != b"\x05\x01\x00\x01":
                raise AssertionError("launcher must request IPv4 SOCKS CONNECT")
            tail = read_exact(client, 6)
            destination = (socket.inet_ntoa(tail[:4]), int.from_bytes(tail[4:], "big"))
            with self.lock:
                self.destinations.append(destination)
            if destination != ("127.0.0.1", 20424):
                raise AssertionError("launcher confused local SOCKS with remote probe destination")
            upstream = socket.create_connection(self.server.address, timeout=3)
            upstream.settimeout(5)
            upstream.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
            fragmented_send(client, self._connect_reply())
            with client.makefile("rb") as client_lines, upstream.makefile("rb") as probe_lines:
                for index in range(4):
                    line = client_lines.readline(4098)
                    if not line:
                        break
                    if not line.endswith(b"\n") or len(line) > 4097:
                        raise AssertionError("launcher request line invalid")
                    message = json.loads(line)
                    if index == 0:
                        if message["operation"] != "hello":
                            raise AssertionError("launcher did not warm up with hello")
                        with self.lock:
                            self.hello_count += 1
                        time.sleep(self.hello_delay)
                    else:
                        if message["operation"] != "connect":
                            raise AssertionError("launcher sent unexpected session operation")
                        time.sleep(self.forwarding_delay)
                    upstream.sendall(line)
                    response = probe_lines.readline(4098)
                    if not response:
                        raise EOFError("actual probe service closed without reply")
                    reply = json.loads(response)
                    if index:
                        with self.lock:
                            self.response_codes.append(reply.get("error", "success"))
                        time.sleep(self.forwarding_delay)
                    fragmented_send(client, response, fragment_size=37)
        except Exception as error:
            if not self.stop_event.is_set():
                with self.lock:
                    self.errors.append(repr(error))
        finally:
            if upstream is not None:
                upstream.close()
            client.close()
            with self.lock:
                self.clients.discard(client)

    def close(self):
        self.stop_event.set()
        self.socks.close()
        self.target.close()
        with self.lock:
            clients = tuple(self.clients)
        for client in clients:
            try:
                client.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
        self.server.request_stop()
        for worker in [self.socks_thread, self.target_thread, self.server_thread] + self.workers:
            worker.join(3)
        alive = [worker.name for worker in [self.socks_thread, self.target_thread,
                                           self.server_thread] + self.workers if worker.is_alive()]
        if alive:
            raise AssertionError("local compatibility fixture threads leaked: " + repr(alive))
        with self.server.lock:
            if self.server.clients or self.server.workers:
                raise AssertionError("actual probe service resources leaked")


@unittest.skipUnless(LAUNCHER_BAT and POWERSHELL,
                     "set MSBOOST_TEST_LAUNCHER_BAT on Windows with Windows PowerShell")
class ActualLauncherCompatibilityTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not Path(LAUNCHER_BAT).is_file():
            raise AssertionError("MSBOOST_TEST_LAUNCHER_BAT does not name a file")

    def invoke_launcher(self, fixture, address=PUBLIC_TARGET):
        environment = dict(os.environ)
        environment.update(MSBOOST_TEST_LAUNCHER_BAT=str(Path(LAUNCHER_BAT).resolve()),
                           MSBOOST_TEST_SOCKS_PORT=str(fixture.socks_port),
                           MSBOOST_TEST_GAME_ADDRESS=address)
        encoded = base64.b64encode(PS_FUNCTION_TEST.encode("utf-16-le")).decode("ascii")
        result = subprocess.run(
            [POWERSHELL, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy",
             "Bypass", "-EncodedCommand", encoded], env=environment,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, encoding="utf-8", timeout=20)
        self.assertEqual(result.returncode, 0, result.stderr)
        payload = json.loads(result.stdout.strip())
        self.assertGreater(payload["FunctionCount"], 20)
        self.assertEqual(payload["Stats"]["TotalCount"], 3)
        self.assertEqual(fixture.destinations, [("127.0.0.1", 20424)])
        self.assertEqual(fixture.session_count, 1, "valid replies must reuse one actual probe session")
        self.assertEqual(fixture.hello_count, 1)
        self.assertFalse(fixture.errors, fixture.errors)
        return payload

    def test_success_samples_include_connect_and_forwarding_exclude_hello(self):
        for reply_form in ("ipv4", "ipv6", "domain"):
            with self.subTest(socks_reply=reply_form):
                fixture = LocalPathFixture(reply_form=reply_form, hello_delay=0.85,
                                           forwarding_delay=0.06, connect_delay=0.14)
                try:
                    result = self.invoke_launcher(fixture)
                    stats = result["Stats"]
                    self.assertEqual(stats["SuccessCount"], 3)
                    self.assertEqual(len(fixture.connect_calls), 3)
                    self.assertEqual(fixture.response_codes, ["success"] * 3)
                    target_deadline = time.monotonic() + 1
                    while len(fixture.target_closes) < 3 and time.monotonic() < target_deadline:
                        time.sleep(0.005)
                    self.assertEqual(fixture.target_closes, [True] * 3)
                    for sample in stats["Samples"]:
                        self.assertTrue(sample["Success"])
                        # 140 ms connector + 60 ms in each forwarding direction.
                        self.assertGreaterEqual(sample["Milliseconds"], 245)
                        # The deliberately 850 ms hello delay must be excluded.
                        self.assertLess(sample["Milliseconds"], 750)
                    samples_total = sum(sample["Milliseconds"] for sample in stats["Samples"])
                    self.assertGreater(result["InvocationMs"] - samples_total, 800)
                    self.assertAlmostEqual(stats["AverageMs"], samples_total / 3, places=6)
                finally:
                    fixture.close()

    def test_valid_refused_replies_reuse_session_and_have_no_average(self):
        fixture = LocalPathFixture(reply_form="domain", outcome="refused")
        try:
            stats = self.invoke_launcher(fixture)["Stats"]
            self.assertEqual(stats["SuccessCount"], 0)
            self.assertIsNone(stats["AverageMs"])
            self.assertEqual(fixture.response_codes, ["refused"] * 3)
            self.assertEqual(len(fixture.connect_calls), 3)
            self.assertEqual(fixture.target_closes, [])
            for sample in stats["Samples"]:
                self.assertFalse(sample["Success"])
                self.assertIsNone(sample["Milliseconds"])
                self.assertEqual(sample["Error"], "游戏端口拒绝连接")
        finally:
            fixture.close()

    def test_disallowed_target_never_calls_connector(self):
        fixture = LocalPathFixture(reply_form="ipv6")
        try:
            stats = self.invoke_launcher(fixture, address="127.0.0.1")["Stats"]
            self.assertEqual(stats["SuccessCount"], 0)
            self.assertIsNone(stats["AverageMs"])
            self.assertEqual(fixture.response_codes, ["target_not_allowed"] * 3)
            self.assertEqual(fixture.connect_calls, [])
            self.assertEqual(fixture.target_closes, [])
            for sample in stats["Samples"]:
                self.assertFalse(sample["Success"])
                self.assertIsNone(sample["Milliseconds"])
                self.assertEqual(sample["Error"], "节点不允许该目标，请使用公网 IPv4 和有效端口")
        finally:
            fixture.close()


if __name__ == "__main__":
    unittest.main(verbosity=2)
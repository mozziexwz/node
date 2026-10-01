"""Local standard-library checks; no public connections or deployment."""

import errno
import importlib.util
import json
import os
import socket
import tempfile
import threading
import time
import unittest
from unittest import mock
import uuid
import sys


SPEC = importlib.util.spec_from_file_location(
    "probe_v2", os.path.join(os.path.dirname(__file__), "tcp_probe.py"))
probe = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(probe)
PUBLIC_TARGET = "35.155.204.207"


def request(operation="hello", **fields):
    value = {"version": 2, "operation": operation, "nonce": uuid.uuid4().hex}
    if operation == "connect":
        value.update(target_address=PUBLIC_TARGET, target_port=8585)
    value.update(fields)
    return value


def wire(value):
    return json.dumps(value, separators=(",", ":")).encode("ascii") + b"\n"


def receive(client):
    line = bytearray()
    while True:
        char = client.recv(1)
        if not char:
            raise EOFError("closed")
        if char == b"\n":
            return json.loads(line)
        line.extend(char)


class ProtocolTests(unittest.TestCase):
    def test_valid_fields_and_crlf(self):
        for value in (request(), request("connect")):
            self.assertEqual(probe.parse_request(wire(value)[:-1]), value)
            self.assertEqual(probe.parse_request(wire(value)[:-1] + b"\r"), value)

    def test_rejects_invalid_shapes_and_types(self):
        original = wire(request("connect"))[:-1]
        malformed = [
            original.replace(b'"version":2', b'"version":2.0'),
            original.replace(b'"version":2', b'"version":2e0'),
            original.replace(b'"version":2', b'"version":true'),
            original.replace(b'"version":2', b'"version":"2"'),
            original.replace(b'"version":2', b'"version":1'),
            original.replace(b'"version":2', b'"Version":2'),
            original.replace(b'"version":2', b'"version":2,"version":2'),
            original.replace(b'"version":2', b'"version":2,"other":false'),
            original.replace(b'"target_port":8585', b'"target_port":false'),
            original.replace(b'"target_port":8585', b'"target_port":8585.0'),
            original.replace(b'"target_port":8585', b'"target_port":8.585e3'),
            original.replace(b'"target_port":8585', b'"target_port":08585'),
            original.replace(b'"target_port":8585', b'"target_port":0'),
            original.replace(b'"target_port":8585', b'"target_port":65536'),
            original.replace(b'"target_port":8585', b'"target_port":null'),
            original.replace(b'"target_port":8585', b'"target_port":{}'),
            original.replace(b'"target_port":8585', b'"target_port":[]'),
            original.replace(b'"connect"', b'"CONNECT"'),
            original.replace(b'"connect"', b'"\\u0063onnect"'),
            original.replace(b'"connect"', b'"con\\nnect"'),
            original.replace(b'"connect"', b'"con\x00nect"'),
            original.replace(b'"target_address":"35.155.204.207"', b'"target_address":"035.155.204.207"'),
            original.replace(b'"target_address":"35.155.204.207"', b'"target_address":"35.155.204"'),
            original.replace(b'"target_address":"35.155.204.207"', b'"target_address":"host.name"'),
            original.replace(b'"target_address":"35.155.204.207"', b'"target_address":"::1"'),
            original.replace(b'"target_address":"35.155.204.207"', b'"target_address":"256.1.1.1"'),
            b"\xef\xbb\xbf" + original,
            b"\xff" + original,
            original + b"x",
            original + b"{}",
            b"[]", b"{}", b"null",
        ]
        for payload in malformed:
            with self.subTest(payload=payload), self.assertRaises(probe.ProtocolError):
                probe.parse_request(payload)
        for nonce in ("", "a" * 31, "a" * 33, "A" * 32, "g" * 32, 1, None):
            with self.subTest(nonce=nonce), self.assertRaises(probe.ProtocolError):
                probe.parse_request(wire(request(nonce=nonce))[:-1])

    def test_line_boundary(self):
        payload = wire(request())[:-1]
        self.assertEqual(probe.parse_request(payload + b" " * (4096 - len(payload)))["version"], 2)
        with self.assertRaises(probe.ProtocolError):
            probe.parse_request(payload + b" " * (4097 - len(payload)))

    def test_stable_public_policy(self):
        denied = ["0.0.0.0", "0.255.255.255", "10.0.0.1", "100.64.0.0", "100.127.255.255",
                  "127.0.0.1", "169.254.1.1", "172.16.0.0", "172.31.255.255", "192.0.0.9",
                  "192.0.2.1", "192.88.99.1", "192.168.1.1", "198.18.0.1", "198.19.255.255",
                  "198.51.100.1", "203.0.113.1", "224.0.0.1", "239.255.255.255", "240.0.0.1",
                  "255.255.255.255"]
        allowed = [PUBLIC_TARGET, "51.222.56.192", "8.8.4.4", "1.1.1.1", "8.8.8.8",
                   "100.63.255.255", "100.128.0.0", "172.15.255.255", "172.32.0.0",
                   "198.17.255.255", "198.20.0.0", "223.255.255.255"]
        for address in denied:
            with self.subTest(address=address):
                self.assertFalse(probe.public_unicast_ipv4(address))
        for address in allowed:
            with self.subTest(address=address):
                self.assertTrue(probe.public_unicast_ipv4(address))

    def test_error_mapping(self):
        cases = [(socket.timeout(), "timeout"), (TimeoutError(), "timeout"),
                 (OSError(errno.ETIMEDOUT, "timeout"), "timeout"),
                 (OSError(errno.ECONNREFUSED, "refused"), "refused"),
                 (OSError(errno.ENETUNREACH, "network"), "unreachable"),
                 (OSError(errno.EHOSTUNREACH, "host"), "unreachable"),
                 (OSError(errno.EINVAL, "other"), "failed")]
        for error, expected in cases:
            self.assertEqual(probe.connect_error(error), expected)

    def test_strict_config(self):
        expected = dict(probe.EXPECTED_CONFIG)
        with tempfile.TemporaryDirectory() as directory:
            path = os.path.join(directory, "probe.json")
            invalid = [dict(expected, listen_address="0.0.0.0"), dict(expected, listen_port=True),
                       dict(expected, listen_port=20425), dict(expected, allowed_peer="127.0.0.2"),
                       dict(expected, extra=1), {}, [], None]
            for config in [expected] + invalid:
                with open(path, "w", encoding="utf-8") as output:
                    json.dump(config, output)
                if config == expected:
                    self.assertEqual(probe.load_config(path), expected)
                else:
                    with self.subTest(config=config), self.assertRaises(ValueError):
                        probe.load_config(path)
            with open(path, "w", encoding="utf-8") as output:
                output.write('{"listen_address":"127.0.0.1","listen_address":"127.0.0.1"}')
            with self.assertRaises(probe.ProtocolError):
                probe.load_config(path)


    def test_lexical_whitespace_and_integer_boundaries(self):
        value = request("connect", target_port=65535)
        payload = wire(value)[:-1]
        self.assertEqual(probe.parse_request(b" \t\r" + payload + b"\t\r "), value)
        malformed = [
            payload.replace(b'"target_port":65535', b'"target_port":"65535"'),
            payload.replace(b'"target_port":65535', b'"target_port":-1'),
            payload.replace(b'"target_port":65535', b'"target_port":+65535'),
            payload.replace(b'"target_port":65535', b'"target_port":1e0'),
            payload.replace(b'"nonce":"', b'"nonce":"\\u0061'),
            payload.replace(b'"nonce":"', b'"nonce":"a\\\\'),
            b"\v" + payload, payload + b"\f", payload + b"\n",
            payload.replace(b'"version":2', b'"version":NaN'),
            payload.replace(b'"version":2', b'"version":Infinity'),
            payload.replace(b'"operation":"connect"', b'"operation":"con\tnect"')]
        for bad in malformed:
            with self.subTest(payload=bad), self.assertRaises(probe.ProtocolError):
                probe.parse_request(bad)
        self.assertEqual(probe.parse_request(wire(request("connect", target_port=1))[:-1])["target_port"], 1)

    def test_fixed_policy_special_ranges_and_canonical_address(self):
        for address in ("192.0.0.8", "192.0.0.9", "192.0.0.10", "192.0.0.11",
                        "192.0.0.170", "192.0.0.171", "192.88.99.2",
                        "169.254.0.0", "169.254.255.255", "198.18.0.0",
                        "198.19.255.255", "224.0.0.0", "239.255.255.255",
                        "240.0.0.0", "255.255.255.255"):
            with self.subTest(address=address):
                self.assertFalse(probe.public_unicast_ipv4(address))
        for address in ("192.31.196.1", "192.52.193.1", "192.175.48.1",
                        "169.253.255.255", "169.255.0.0", "192.0.1.0",
                        "223.255.255.255"):
            with self.subTest(address=address):
                self.assertTrue(probe.public_unicast_ipv4(address))
        for address in ("01.1.1.1", "1.01.1.1", "1.1.1.01", "1.1.1.1 ",
                        "1.1.1.1\n", "1.1.1.1.1", "1..1.1", "+1.1.1.1",
                        "1.1.1.256", "１.1.1.1", 0, None):
            with self.subTest(address=address), self.assertRaises(probe.ProtocolError):
                probe.canonical_ipv4(address)

    def test_fixed_config_rejects_all_changed_limits_and_type_substitutions(self):
        with tempfile.TemporaryDirectory() as directory:
            path = os.path.join(directory, "probe.json")
            for name, original in probe.EXPECTED_CONFIG.items():
                if type(original) is int:
                    for substitute in (original + 1, float(original), True, str(original), None):
                        value = dict(probe.EXPECTED_CONFIG)
                        value[name] = substitute
                        with open(path, "w", encoding="utf-8") as output:
                            json.dump(value, output)
                        with self.subTest(field=name, value=substitute), self.assertRaises(ValueError):
                            probe.load_config(path)

    def test_validate_config_cli_does_not_open_any_socket(self):
        with tempfile.TemporaryDirectory() as directory:
            path = os.path.join(directory, "probe.json")
            with open(path, "w", encoding="utf-8") as output:
                json.dump(probe.EXPECTED_CONFIG, output)
            with mock.patch.object(sys, "argv", ["tcp_probe.py", "--config", path, "--validate-config"]), \
                    mock.patch.object(probe.socket, "socket", side_effect=AssertionError("bound during validation")):
                self.assertEqual(probe.main(), 0)


class SessionTests(unittest.TestCase):
    def start_server(self, connector=None, limits=None):
        options = {"listen_port": 0}
        if connector is not None:
            options["connector"] = connector
        if limits is not None:
            options["limits"] = limits
        server = probe.ProbeServer(**options)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()

        def finish():
            server.request_stop()
            thread.join(3)
            self.assertFalse(thread.is_alive(), "server thread did not stop")
            with server.lock:
                self.assertFalse(server.clients, "client resources leaked")
                self.assertFalse(server.workers, "worker resources leaked")
        self.addCleanup(finish)
        return server

    def open_client(self, server):
        client = socket.create_connection(server.address, timeout=1)
        self.addCleanup(client.close)
        return client

    def hello(self, client, server):
        message = request()
        client.sendall(wire(message))
        reply = receive(client)
        self.assertEqual(reply, dict(message, node_id=server.node_id, success=True))
        return message

    def assert_closed(self, client):
        try:
            self.assertEqual(client.recv(1), b"")
        except (ConnectionResetError, ConnectionAbortedError):
            pass

    def test_real_connect_no_game_handshake_and_resource_release(self):
        target = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        target.bind(("127.0.0.1", 0))
        target.listen(3)
        target.settimeout(2)
        self.addCleanup(target.close)
        closed = []
        target_done = threading.Event()

        def accept_targets():
            try:
                for unused in range(3):
                    connection, address = target.accept()
                    with connection:
                        connection.settimeout(1)
                        closed.append(connection.recv(1) == b"")
            finally:
                target_done.set()

        target_thread = threading.Thread(target=accept_targets, daemon=True)
        target_thread.start()
        calls = []

        def mapped_connector(address, port, timeout):
            calls.append((address, port, timeout))
            probe.tcp_connect("127.0.0.1", target.getsockname()[1], timeout)

        server = self.start_server(mapped_connector)
        client = self.open_client(server)
        self.hello(client, server)
        for unused in range(3):
            message = request("connect")
            start = time.monotonic()
            client.sendall(wire(message))
            self.assertEqual(receive(client), dict(message, node_id=server.node_id, success=True))
            self.assertLess(time.monotonic() - start, 0.5)
        self.assert_closed(client)
        self.assertTrue(target_done.wait(1))
        target_thread.join(1)
        self.assertEqual(closed, [True, True, True])
        self.assertEqual(len(calls), 3)
        for address, port, timeout in calls:
            self.assertEqual((address, port), (PUBLIC_TARGET, 8585))
            self.assertGreater(timeout, 0)
            self.assertLessEqual(timeout, 2)

    def test_reply_waits_for_connect_completion(self):
        entered = threading.Event()
        release = threading.Event()

        def controlled_connect(address, port, timeout):
            entered.set()
            if not release.wait(timeout):
                raise socket.timeout()

        server = self.start_server(controlled_connect)
        client = self.open_client(server)
        self.hello(client, server)
        message = request("connect")
        client.sendall(wire(message))
        self.assertTrue(entered.wait(1))
        client.settimeout(0.08)
        with self.assertRaises(socket.timeout):
            client.recv(1)
        release.set()
        client.settimeout(1)
        self.assertEqual(receive(client), dict(message, node_id=server.node_id, success=True))

    def test_valid_errors_keep_session_and_count_attempts(self):
        errors = [OSError(errno.ECONNREFUSED, "refused"), socket.timeout(),
                  OSError(errno.EHOSTUNREACH, "unreachable")]

        def failing_connect(address, port, timeout):
            raise errors.pop(0)

        server = self.start_server(failing_connect)
        client = self.open_client(server)
        self.hello(client, server)
        for code in ("refused", "timeout", "unreachable"):
            message = request("connect")
            client.sendall(wire(message))
            self.assertEqual(receive(client), dict(message, node_id=server.node_id, success=False, error=code))
        self.assert_closed(client)

    def test_real_refused_and_generic_failed(self):
        unused = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        unused.bind(("127.0.0.1", 0))
        unused_port = unused.getsockname()[1]
        unused.close()

        def refused_connect(address, port, timeout):
            probe.tcp_connect("127.0.0.1", unused_port, timeout)

        # Windows' closed-port SYN retry can take >2s. Use a larger test-only
        # limit to exercise the real ECONNREFUSED mapping; production stays 2s.
        server = self.start_server(refused_connect, probe.DEFAULT_LIMITS._replace(connect=4.0))
        client = self.open_client(server)
        self.hello(client, server)
        # Windows may retry a loopback SYN before returning ECONNREFUSED.
        client.settimeout(5)
        client.sendall(wire(request("connect")))
        self.assertEqual(receive(client)["error"], "refused")

        def generic_connect(address, port, timeout):
            raise OSError(errno.EINVAL, "failed")

        other = self.start_server(generic_connect)
        client = self.open_client(other)
        self.hello(client, other)
        client.sendall(wire(request("connect")))
        self.assertEqual(receive(client)["error"], "failed")

    def test_private_target_rejected_without_connect(self):
        calls = []
        server = self.start_server(lambda *args: calls.append(args))
        client = self.open_client(server)
        self.hello(client, server)
        message = request("connect", target_address="127.0.0.1")
        client.sendall(wire(message))
        self.assertEqual(receive(client), dict(message, node_id=server.node_id,
                                              success=False, error="target_not_allowed"))
        self.assertFalse(calls)

    def test_fragmented_requests_and_crlf(self):
        server = self.start_server(lambda *args: None)
        client = self.open_client(server)
        hello = request()
        for char in wire(hello)[:-1] + b"\r\n":
            client.sendall(bytes((char,)))
        self.assertEqual(receive(client)["nonce"], hello["nonce"])
        message = request("connect")
        for char in wire(message):
            client.sendall(bytes((char,)))
        self.assertEqual(receive(client)["nonce"], message["nonce"])

    def test_malformed_request_and_order_do_not_connect(self):
        calls = []
        server = self.start_server(lambda *args: calls.append(args))
        messages = [wire(request("connect")), wire(request()) + wire(request()),
                    b"x" * 4097 + b"\n", b'\xef\xbb\xbf' + wire(request()),
                    wire(dict(request(), unknown=1))]
        for payload in messages:
            with self.subTest(payload=payload[:80]):
                client = self.open_client(server)
                client.sendall(payload)
                self.assert_closed(client)
                client.close()
        self.assertFalse(calls)

    def test_repeated_nonce_and_repeated_hello_rejected(self):
        calls = []
        server = self.start_server(lambda *args: calls.append(args))
        client = self.open_client(server)
        original = self.hello(client, server)
        client.sendall(wire(request("connect", nonce=original["nonce"])))
        self.assert_closed(client)
        client = self.open_client(server)
        self.hello(client, server)
        client.sendall(wire(request()))
        self.assert_closed(client)
        client = self.open_client(server)
        self.hello(client, server)
        connect = request("connect")
        client.sendall(wire(connect))
        self.assertTrue(receive(client)["success"])
        client.sendall(wire(connect))
        self.assert_closed(client)
        self.assertEqual(len(calls), 1)

    def test_slow_fragments_do_not_extend_idle_deadline(self):
        limits = probe.DEFAULT_LIMITS._replace(idle=0.15)
        server = self.start_server(lambda *args: None, limits)
        client = self.open_client(server)
        start = time.monotonic()
        for fragment in (b"{", b'"version"', b":"):
            client.sendall(fragment)
            time.sleep(0.065)
        self.assert_closed(client)
        self.assertLess(time.monotonic() - start, 0.4)

    def test_total_lifetime_caps_read_and_connect(self):
        timeouts = []
        limits = probe.DEFAULT_LIMITS._replace(idle=1.0, lifetime=0.20, connect=1.0)

        def connect_at_deadline(address, port, timeout):
            timeouts.append(timeout)
            time.sleep(timeout + 0.01)
            raise socket.timeout()

        server = self.start_server(connect_at_deadline, limits)
        client = self.open_client(server)
        self.hello(client, server)
        time.sleep(0.08)
        client.sendall(wire(request("connect")))
        start = time.monotonic()
        self.assert_closed(client)
        self.assertLess(time.monotonic() - start, 0.3)
        self.assertEqual(len(timeouts), 1)
        self.assertGreater(timeouts[0], 0)
        self.assertLess(timeouts[0], 0.15)

    def test_late_connect_cannot_report_success(self):
        limits = probe.DEFAULT_LIMITS._replace(connect=0.06)

        def late_connect(address, port, timeout):
            time.sleep(0.08)

        server = self.start_server(late_connect, limits)
        client = self.open_client(server)
        self.hello(client, server)
        client.sendall(wire(request("connect")))
        self.assertEqual(receive(client)["error"], "timeout")

    def test_concurrency_limit_and_released_slots(self):
        server = self.start_server(lambda *args: None)
        clients = [self.open_client(server) for unused in range(4)]
        for client in clients:
            self.hello(client, server)
        excess = self.open_client(server)
        self.assert_closed(excess)
        clients[0].close()
        deadline = time.monotonic() + 1
        while time.monotonic() < deadline:
            with server.lock:
                if len(server.clients) == 3:
                    break
            time.sleep(0.005)
        replacement = self.open_client(server)
        self.hello(replacement, server)

    def test_shutdown_during_worker_start_releases_every_resource(self):
        server = self.start_server(lambda *args: None)
        startup_entered = threading.Event()
        allow_start = threading.Event()
        original_start = threading.Thread.start

        def paused_start(worker):
            startup_entered.set()
            if not allow_start.wait(1):
                raise RuntimeError("test start gate expired")
            return original_start(worker)

        with mock.patch.object(threading.Thread, "start", paused_start):
            client = self.open_client(server)
            self.assertTrue(startup_entered.wait(1))
            stop_thread = threading.Thread(target=server.request_stop)
            original_start(stop_thread)
            self.assertTrue(server.stop_event.wait(1))
            allow_start.set()
            stop_thread.join(1)
            self.assertFalse(stop_thread.is_alive())
        self.assert_closed(client)

    def test_shutdown_reentrant_at_worker_start_does_not_deadlock(self):
        server = self.start_server(lambda *args: None)
        original_start = threading.Thread.start
        stopped = threading.Event()

        def reentrant_start(worker):
            # Emulates SIGTERM delivered on the acceptance thread in the
            # middle of registration; request_stop takes the same RLock.
            server.request_stop()
            stopped.set()
            return original_start(worker)

        with mock.patch.object(threading.Thread, "start", reentrant_start):
            client = self.open_client(server)
            self.assertTrue(stopped.wait(1))
        self.assert_closed(client)

    def test_non_127_0_0_1_peer_is_rejected(self):
        calls = []
        server = self.start_server(lambda *args: calls.append(args))
        client = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.addCleanup(client.close)
        client.settimeout(1)
        client.bind(("127.0.0.2", 0))
        client.connect(server.address)
        self.assert_closed(client)
        self.assertFalse(calls)

    def test_process_identity_changes_only_between_instances(self):
        server = self.start_server(lambda *args: None)
        other = self.start_server(lambda *args: None)
        self.assertRegex(server.node_id, r"^[0-9a-f]{32}$")
        self.assertNotEqual(server.node_id, other.node_id)
        for unused in range(2):
            client = self.open_client(server)
            self.hello(client, server)
            client.sendall(wire(request("connect")))
            self.assertEqual(receive(client)["node_id"], server.node_id)
            client.close()


class FakeStream:
    def __init__(self, clock, advance):
        self.clock = clock
        self.advance = advance
        self.timeout = None
        self.wire = None

    def settimeout(self, timeout):
        self.timeout = timeout

    def sendall(self, data):
        self.wire = data
        self.clock[0] += self.advance


class DeadlineTests(unittest.TestCase):
    def test_reply_write_uses_remaining_lifetime_and_rejects_late_return(self):
        now = [10.0]
        stream = FakeStream(now, 0.11)
        with self.assertRaises(socket.timeout):
            probe.write_reply(stream, dict(request(), node_id=uuid.uuid4().hex, success=True),
                              10.10, lambda: now[0])
        self.assertAlmostEqual(stream.timeout, 0.10)
        self.assertTrue(stream.wire.endswith(b"\n"))

    def test_already_expired_deadline_does_no_io(self):
        now = [10.0]
        stream = FakeStream(now, 0)
        with self.assertRaises(socket.timeout):
            probe.write_reply(stream, request(), 10.0, lambda: now[0])
        self.assertIsNone(stream.wire)


class MemoryReader:
    def __init__(self, payload, fragment_size=512, now=None, advance=0):
        self.payload = payload
        self.fragment_size = fragment_size
        self.now = now
        self.advance = advance
        self.timeouts = []

    def settimeout(self, timeout):
        self.timeouts.append(timeout)

    def recv(self, count):
        count = min(count, self.fragment_size)
        chunk, self.payload = self.payload[:count], self.payload[count:]
        if self.now is not None:
            self.now[0] += self.advance
        return chunk


class FramingTests(unittest.TestCase):
    def test_exact_4096_byte_limit_includes_cr_but_excludes_lf(self):
        value = request()
        payload = wire(value)[:-1]
        for ending in (b"\n", b"\r\n"):
            line = payload + b" " * (4096 - len(payload) - (len(ending) - 1)) + ending
            stream = MemoryReader(line, fragment_size=17)
            parsed = probe.parse_request(probe.read_line(stream, time.monotonic() + 1))
            self.assertEqual(parsed, value)
            self.assertEqual(stream.payload, b"")
            stream = MemoryReader(b" " + line, fragment_size=1)
            with self.assertRaises(probe.ProtocolError):
                probe.read_line(stream, time.monotonic() + 1)

    def test_coalesced_or_partial_next_request_is_not_discarded(self):
        payload = wire(request())
        for extra in (wire(request()), b"{", b" "):
            stream = MemoryReader(payload + extra)
            with self.subTest(extra=extra), self.assertRaises(probe.ProtocolError):
                probe.read_line(stream, time.monotonic() + 1)

    def test_fragment_timeout_uses_absolute_deadline_and_checks_late_return(self):
        now = [10.0]
        stream = MemoryReader(wire(request()), fragment_size=1, now=now, advance=0.035)
        with self.assertRaises(socket.timeout):
            probe.read_line(stream, 10.10, lambda: now[0])
        self.assertEqual(len(stream.timeouts), 3)
        for previous, current in zip(stream.timeouts, stream.timeouts[1:]):
            self.assertLess(current, previous)

    def test_eof_during_fragment_closes_line(self):
        stream = MemoryReader(b'{"version":2', fragment_size=1)
        with self.assertRaises(EOFError):
            probe.read_line(stream, time.monotonic() + 1)


if __name__ == "__main__":
    unittest.main(verbosity=2)

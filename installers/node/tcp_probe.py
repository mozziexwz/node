#!/usr/bin/env python3
# Managed by msboost-installer-v1: tcp-probe-v2
"""MSBOOST v2 full-path TCP probe, Python 3.7+, standard library only.

The production command accepts only the fixed loopback configuration. Targets
arrive over an authenticated MSBOOST path and are checked against a stable IPv4
policy. A successful reply means a real TCP connect completed; game data is
never read. All durations are measured by the client, not returned here.
"""

import argparse
import collections
import errno
import json
import logging
import re
import signal
import socket
import threading
import time
import uuid


MAX_LINE_BYTES = 4096
Limits = collections.namedtuple(
    "Limits", "idle lifetime connect write max_sessions max_connects",
    defaults=(3.0, 15.0, 2.0, 2.0, 4, 3))
DEFAULT_LIMITS = Limits()
NONCE = re.compile(r"\A[0-9a-f]{32}\Z")
SCALAR = r'(?:"[A-Za-z0-9_.-]*"|true|false|-?(?:0|[1-9][0-9]*))'
FIELD = r'"[A-Za-z0-9_.-]*"[ \t\r]*:[ \t\r]*' + SCALAR
FLAT_JSON = re.compile(r'\A[ \t\r]*\{[ \t\r]*' + FIELD +
                       r'(?:[ \t\r]*,[ \t\r]*' + FIELD +
                       r')*[ \t\r]*\}[ \t\r]*\Z')
HELLO_FIELDS = frozenset(("version", "operation", "nonce"))
CONNECT_FIELDS = HELLO_FIELDS | frozenset(("target_address", "target_port"))

IPV4 = re.compile(r"\A(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})\."
                  r"(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})\Z")

# Fixed numeric prefix policy; no Python-version-dependent is_global/private
# classification or DNS lookup. Entire special-purpose/deprecated blocks are
# rejected conservatively, including 192.0.0.9/10 anycast exceptions.
DENIED_IPV4 = (
    (0x00000000, 8), (0x0a000000, 8), (0x64400000, 10),
    (0x7f000000, 8), (0xa9fe0000, 16), (0xac100000, 12),
    (0xc0000000, 24), (0xc0000200, 24), (0xc0586300, 24),
    (0xc0a80000, 16), (0xc6120000, 15), (0xc6336400, 24),
    (0xcb007100, 24), (0xe0000000, 4), (0xf0000000, 4))

# Production values are fixed, rather than accepting user-controlled budgets,
# additional peers, or listener addresses through a configuration file.
EXPECTED_CONFIG = {
    "listen_address": "127.0.0.1", "listen_port": 20424,
    "allowed_peer": "127.0.0.1", "max_sessions": 4,
    "max_connect_requests": 3, "idle_timeout": 3,
    "session_lifetime": 15, "connect_timeout": 2,
    "write_timeout": 2, "max_line_bytes": MAX_LINE_BYTES}


class ProtocolError(ValueError):
    """The connection must be discarded without probing a target."""


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ProtocolError("duplicate field")
        result[key] = value
    return result


def canonical_ipv4(value):
    if type(value) is not str:
        raise ProtocolError("invalid IPv4 type")
    match = IPV4.fullmatch(value)
    if match is None:
        raise ProtocolError("invalid IPv4")
    octets = tuple(int(part) for part in match.groups())
    if any(part > 255 for part in octets):
        raise ProtocolError("invalid IPv4 octet")
    address = 0
    for part in octets:
        address = (address << 8) | part
    return address


def public_unicast_ipv4(value):
    address = canonical_ipv4(value)
    return not any(address >> (32 - prefix) == network >> (32 - prefix)
                   for network, prefix in DENIED_IPV4)


def parse_request(payload):
    if len(payload) > MAX_LINE_BYTES:
        raise ProtocolError("line too long")
    try:
        text = payload.decode("utf-8", "strict")
    except UnicodeDecodeError:
        raise ProtocolError("invalid UTF-8")
    if FLAT_JSON.fullmatch(text) is None:
        raise ProtocolError("invalid flat JSON")
    try:
        request = json.loads(text, object_pairs_hook=unique_object)
    except (ValueError, TypeError) as error:
        raise ProtocolError(str(error))
    if type(request.get("version")) is not int or request["version"] != 2:
        raise ProtocolError("invalid version")
    if not isinstance(request.get("nonce"), str) or NONCE.fullmatch(request["nonce"]) is None:
        raise ProtocolError("invalid nonce")
    operation = request.get("operation")
    if operation == "hello":
        fields = HELLO_FIELDS
    elif operation == "connect":
        fields = CONNECT_FIELDS
        canonical_ipv4(request.get("target_address"))
        port = request.get("target_port")
        if type(port) is not int or not 1 <= port <= 65535:
            raise ProtocolError("invalid target port")
    else:
        raise ProtocolError("invalid operation")
    if frozenset(request) != fields:
        raise ProtocolError("unknown or missing field")
    return request


def remaining(deadline, clock=time.monotonic):
    timeout = deadline - clock()
    if timeout <= 0:
        raise socket.timeout("deadline exceeded")
    return timeout


def read_line(stream, deadline, clock=time.monotonic):
    payload = bytearray()
    while True:
        stream.settimeout(remaining(deadline, clock))
        chunk = stream.recv(min(512, MAX_LINE_BYTES + 1 - len(payload)))
        # A late successful OS return still cannot exceed the shared deadline.
        remaining(deadline, clock)
        if not chunk:
            raise EOFError("peer closed connection")
        newline = chunk.find(b"\n")
        if newline >= 0:
            if newline != len(chunk) - 1:
                raise ProtocolError("unsolicited bytes after line")
            payload.extend(chunk[:newline])
            if len(payload) > MAX_LINE_BYTES:
                raise ProtocolError("line too long")
            return bytes(payload)
        payload.extend(chunk)
        if len(payload) > MAX_LINE_BYTES:
            raise ProtocolError("line too long")


def write_reply(stream, reply, deadline, clock=time.monotonic):
    wire = json.dumps(reply, ensure_ascii=True, separators=(",", ":")).encode("ascii") + b"\n"
    stream.settimeout(remaining(deadline, clock))
    stream.sendall(wire)
    remaining(deadline, clock)


def tcp_connect(address, port, timeout):
    # Numeric canonical IPv4 + an AF_INET socket: no DNS and no application IO.
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as target:
        target.settimeout(timeout)
        target.connect((address, port))


def connect_error(error):
    if isinstance(error, (socket.timeout, TimeoutError)) or getattr(error, "errno", None) == errno.ETIMEDOUT:
        return "timeout"
    code = getattr(error, "errno", None)
    if code == errno.ECONNREFUSED:
        return "refused"
    if code in (errno.ENETUNREACH, errno.EHOSTUNREACH, errno.ENETDOWN,
                getattr(errno, "EHOSTDOWN", -1), errno.EADDRNOTAVAIL):
        return "unreachable"
    return "failed"


class ProbeServer:
    """Bounded loopback server. Constructor seams are for local unit tests only."""

    def __init__(self, listen_address="127.0.0.1", listen_port=20424,
                 connector=tcp_connect, limits=DEFAULT_LIMITS, clock=time.monotonic):
        if listen_address != "127.0.0.1" or type(listen_port) is not int or not 0 <= listen_port <= 65535:
            raise ValueError("loopback IPv4 binding required")
        self.node_id = uuid.uuid4().hex
        self.connector = connector
        self.limits = limits
        self.clock = clock
        self.stop_event = threading.Event()
        self.slots = threading.BoundedSemaphore(limits.max_sessions)
        # A Python signal handler may request_stop on this same thread while
        # acceptance is being registered. Reentrancy avoids a signal deadlock.
        self.lock = threading.RLock()
        self.clients = set()
        self.workers = set()
        self.listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        try:
            self.listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            self.listener.bind((listen_address, listen_port))
            self.listener.listen(limits.max_sessions)
            self.listener.settimeout(0.25)
            self.address = self.listener.getsockname()
        except BaseException:
            self.listener.close()
            raise

    def serve_forever(self):
        try:
            while not self.stop_event.is_set():
                try:
                    client, peer = self.listener.accept()
                except socket.timeout:
                    continue
                except OSError:
                    if self.stop_event.is_set():
                        break
                    raise
                if peer[0] != "127.0.0.1" or not self.slots.acquire(blocking=False):
                    client.close()
                    continue
                deadline = self.clock() + self.limits.lifetime
                worker = threading.Thread(target=self._run_session, args=(client, deadline), daemon=True)
                with self.lock:
                    if self.stop_event.is_set():
                        client.close()
                        self.slots.release()
                        break
                    self.clients.add(client)
                    self.workers.add(worker)
                    # Stop/join snapshots cannot observe an unstarted worker.
                    try:
                        worker.start()
                    except BaseException:
                        self.clients.discard(client)
                        self.workers.discard(worker)
                        client.close()
                        self.slots.release()
                        raise
        finally:
            self.request_stop()
            self.join_workers()

    def _run_session(self, client, deadline):
        try:
            client.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
            self._handle_session(client, deadline)
        except (ProtocolError, EOFError, OSError, ValueError):
            # Malformed/expired sessions close without exposing arbitrary text.
            pass
        finally:
            client.close()
            with self.lock:
                self.clients.discard(client)
                self.workers.discard(threading.current_thread())
            self.slots.release()

    def _handle_session(self, client, deadline):
        seen_nonces = set()
        hello = parse_request(read_line(client, min(deadline, self.clock() + self.limits.idle), self.clock))
        if hello["operation"] != "hello":
            raise ProtocolError("hello required")
        seen_nonces.add(hello["nonce"])
        self._reply(client, hello, True, deadline)
        for unused in range(self.limits.max_connects):
            request = parse_request(read_line(client, min(deadline, self.clock() + self.limits.idle), self.clock))
            if request["operation"] != "connect" or request["nonce"] in seen_nonces:
                raise ProtocolError("invalid session order or repeated nonce")
            seen_nonces.add(request["nonce"])
            if not public_unicast_ipv4(request["target_address"]):
                self._reply(client, request, False, deadline, "target_not_allowed")
                continue
            try:
                connect_deadline = min(deadline, self.clock() + self.limits.connect)
                self.connector(request["target_address"], request["target_port"],
                               remaining(connect_deadline, self.clock))
                remaining(connect_deadline, self.clock)
            except OSError as error:
                self._reply(client, request, False, deadline, connect_error(error))
            else:
                self._reply(client, request, True, deadline)

    def _reply(self, client, request, success, deadline, error=None):
        reply = dict(request)
        reply["node_id"] = self.node_id
        reply["success"] = success
        if error is not None:
            reply["error"] = error
        write_reply(client, reply, min(deadline, self.clock() + self.limits.write), self.clock)

    def request_stop(self):
        self.stop_event.set()
        self.listener.close()
        with self.lock:
            clients = tuple(self.clients)
        for client in clients:
            try:
                client.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass

    def join_workers(self):
        deadline = time.monotonic() + self.limits.connect + 0.5
        with self.lock:
            workers = tuple(self.workers)
        for worker in workers:
            worker.join(max(0, deadline - time.monotonic()))


def load_config(path):
    with open(path, "r", encoding="utf-8") as source:
        config = json.load(source, object_pairs_hook=unique_object)
    if type(config) is not dict or set(config) != set(EXPECTED_CONFIG):
        raise ValueError("unknown or missing configuration field")
    for key, expected in EXPECTED_CONFIG.items():
        if type(config[key]) is not type(expected) or config[key] != expected:
            raise ValueError("fixed loopback configuration and limits required")
    return config


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", required=True)
    parser.add_argument("--validate-config", action="store_true",
                        help="validate fixed configuration without binding a socket")
    arguments = parser.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(message)s")
    try:
        config = load_config(arguments.config)
        if arguments.validate_config:
            return 0
        server = ProbeServer(config["listen_address"], config["listen_port"])
    except (OSError, ValueError) as error:
        logging.error("Unable to start v2 probe: %s", error)
        return 1

    def stop(signum, frame):
        server.request_stop()

    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGTERM, stop)
    logging.info("MSBOOST TCP probe v2 listening on 127.0.0.1:20424")
    server.serve_forever()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

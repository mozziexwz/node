"""Mandatory plaintext SOCKS screen for an MSBOOST-managed free relay port.

The public socket belongs to this process; the pinned GOST child listens only
on 127.0.0.1 and retains target forwarding and rate limiting. This file is
embedded in the server binary and installed as a systemd credential.
"""

import json
import selectors
import signal
import socket
import subprocess
import sys
import threading
import time


def _terminate(_signum, _frame):
    raise SystemExit(0)


signal.signal(signal.SIGTERM, _terminate)
signal.signal(signal.SIGINT, _terminate)


def _read_exact(conn, count, prefix):
    while count:
        part = conn.recv(count)
        if not part:
            raise ConnectionError("incomplete protocol greeting")
        prefix.extend(part)
        count -= len(part)


def screen_socks(conn):
    conn.settimeout(5)
    prefix = bytearray()
    try:
        _read_exact(conn, 1, prefix)
        if prefix[0] == 4:
            _read_exact(conn, 1, prefix)
            if prefix[1] not in (1, 2):
                return bytes(prefix), False
            _read_exact(conn, 6, prefix)
            return bytes(prefix), prefix[2:4] != b"\x00\x00"
        elif prefix[0] == 5:
            _read_exact(conn, 1, prefix)
            count = prefix[1]
            if not count or count > 16:
                # Long arbitrary lists overlap random Mieru opening bytes.
                return bytes(prefix), False
            _read_exact(conn, count, prefix)
            methods = prefix[2:]
            return bytes(prefix), 0 in methods or 2 in methods
        return bytes(prefix), False
    finally:
        conn.settimeout(None)


def _serve_connection(client, backend_port, active, active_lock, permits):
    upstream = None
    try:
        prefix, blocked = screen_socks(client)
        if blocked:
            return
        upstream = socket.create_connection(("127.0.0.1", backend_port), timeout=3)
        client.settimeout(30)
        upstream.settimeout(30)
        upstream.sendall(prefix)
        with selectors.DefaultSelector() as selector:
            selector.register(client, selectors.EVENT_READ)
            selector.register(upstream, selectors.EVENT_READ)
            while selector.get_map():
                for key, _ in selector.select(timeout=1):
                    source = key.fileobj
                    destination = upstream if source is client else client
                    chunk = source.recv(65536)
                    if chunk:
                        destination.sendall(chunk)
                    else:
                        selector.unregister(source)
                        try:
                            destination.shutdown(socket.SHUT_WR)
                        except OSError:
                            pass
    except (OSError, ValueError, ConnectionError):
        pass
    finally:
        if upstream is not None:
            upstream.close()
        client.close()
        with active_lock:
            active.discard(client)
        permits.release()


def _public_listener(port):
    for family, address in ((socket.AF_INET6, ("::", port)),
                            (socket.AF_INET, ("0.0.0.0", port))):
        listener = None
        try:
            listener = socket.socket(family, socket.SOCK_STREAM)
            listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            if family == socket.AF_INET6:
                listener.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 0)
            listener.bind(address)
            return listener
        except OSError:
            if listener is not None:
                listener.close()
    raise OSError("public relay port is unavailable")


def main():
    if len(sys.argv) != 4:
        return 2
    binary, gost_config_path, guard_config_path = sys.argv[1:]
    with open(guard_config_path, encoding="utf-8") as stream:
        guard_config = json.load(stream)
    public_port = guard_config.get("publicPort")
    backend_port = guard_config.get("backendPort")
    if (type(public_port) is not int or type(backend_port) is not int
            or not 1 <= public_port <= 65535
            or not 1 <= backend_port <= 65535
            or public_port == backend_port):
        return 2
    with open(gost_config_path, encoding="utf-8") as stream:
        gost_config = json.load(stream)
    services = gost_config.get("services", [])
    if (len(services) != 1 or services[0].get("name") != "msboost-free"
            or services[0].get("addr") != "127.0.0.1:" + str(backend_port)):
        return 2

    listener = _public_listener(public_port)
    child = None
    active = set()
    active_lock = threading.Lock()
    permits = threading.BoundedSemaphore(4096)
    try:
        child = subprocess.Popen([binary, "-C", gost_config_path],
                                 stdin=subprocess.DEVNULL,
                                 stdout=subprocess.DEVNULL,
                                 stderr=subprocess.DEVNULL,
                                 close_fds=True)
        deadline = time.monotonic() + 8
        while time.monotonic() < deadline:
            if child.poll() is not None:
                return 1
            try:
                with socket.create_connection(("127.0.0.1", backend_port),
                                              timeout=0.2):
                    break
            except OSError:
                time.sleep(0.05)
        else:
            return 1

        listener.listen(256)
        listener.settimeout(1)
        while child.poll() is None:
            try:
                client, _ = listener.accept()
            except socket.timeout:
                continue
            if not permits.acquire(blocking=False):
                client.close()
                continue
            with active_lock:
                active.add(client)
            threading.Thread(target=_serve_connection,
                             args=(client, backend_port, active, active_lock,
                                   permits), daemon=True).start()
        return 1
    finally:
        listener.close()
        with active_lock:
            for conn in active:
                conn.close()
        if child is not None and child.poll() is None:
            child.terminate()
            try:
                child.wait(timeout=3)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait(timeout=3)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, TypeError, KeyError, json.JSONDecodeError):
        sys.exit(1)

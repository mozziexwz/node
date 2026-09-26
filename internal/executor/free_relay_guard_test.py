"""Protocol-screen tests for the Python code embedded in the free installer."""

import socket
import threading
import unittest

import free_relay_guard


class FreeRelayGuardTest(unittest.TestCase):
    def check(self, parts, expected_blocked):
        writer, reader = socket.socketpair()
        sent = b"".join(parts)

        def send():
            try:
                for part in parts:
                    writer.sendall(part)
            except OSError:
                pass
            finally:
                writer.close()

        thread = threading.Thread(target=send)
        thread.start()
        try:
            prefix, blocked = free_relay_guard.screen_socks(reader)
            self.assertEqual(blocked, expected_blocked)
            if not blocked:
                remainder = bytearray()
                while True:
                    part = reader.recv(4096)
                    if not part:
                        break
                    remainder.extend(part)
                self.assertEqual(prefix + remainder, sent)
        finally:
            reader.close()
            thread.join(timeout=2)

    def test_fragmented_socks5(self):
        self.check([b"\x05", b"\x02", b"\x7f", b"\x00"], True)

    def test_socks5_gssapi(self):
        self.check([b"\x05", b"\x01", b"\x01"], True)

    def test_fragmented_socks4(self):
        self.check([b"\x04", b"\x01\x00\x50", b"\x7f\x00\x00\x01"], True)

    def test_game_payload(self):
        self.check([b"\x15", b"\x38 Mieru game traffic"], False)

    def test_large_random_mieru_prefix(self):
        self.check([b"\x05\xc8", b"\x00\x02\x31" * 70], False)

    def test_small_non_socks_methods(self):
        self.check([b"\x05\x02\x31\x42", b"more game traffic"], False)

    def test_backend_eof_releases_worker_without_client_close(self):
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as backend:
            backend.bind(("127.0.0.1", 0))
            backend.listen(1)

            def close_backend():
                conn, _ = backend.accept()
                with conn:
                    conn.recv(1)

            backend_thread = threading.Thread(target=close_backend)
            backend_thread.start()
            client, guarded = socket.socketpair()
            active = {guarded}
            active_lock = threading.Lock()
            permits = threading.BoundedSemaphore(1)
            self.assertTrue(permits.acquire(blocking=False))
            worker = threading.Thread(
                target=free_relay_guard._serve_connection,
                args=(guarded, backend.getsockname()[1], active,
                      active_lock, permits),
            )
            worker.start()
            try:
                client.settimeout(2)
                client.sendall(b"game traffic")
                self.assertEqual(client.recv(1), b"")
                worker.join(timeout=2)
                self.assertFalse(worker.is_alive(), "backend EOF pinned a worker")
                self.assertEqual(active, set())
                self.assertTrue(permits.acquire(blocking=False))
            finally:
                client.close()
                guarded.close()
                backend_thread.join(timeout=2)


if __name__ == "__main__":
    unittest.main()

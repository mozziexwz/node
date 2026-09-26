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

    def test_fragmented_socks4(self):
        self.check([b"\x04", b"\x01\x00\x50", b"\x7f\x00\x00\x01"], True)

    def test_game_payload(self):
        self.check([b"\x15", b"\x38 Mieru game traffic"], False)

    def test_large_random_mieru_prefix(self):
        self.check([b"\x05\xc8", b"\x00\x02\x31" * 70], False)

    def test_small_non_socks_methods(self):
        self.check([b"\x05\x02\x31\x42", b"more game traffic"], False)


if __name__ == "__main__":
    unittest.main()

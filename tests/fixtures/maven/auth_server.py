"""Local Maven repository requiring the fixture's decrypted credentials."""
import base64
import functools
import http.server
import sys


class AuthRepository(http.server.SimpleHTTPRequestHandler):
    def do_GET(self):
        expected = "Basic " + base64.b64encode(b"fixture:fixture-password").decode("ascii")
        if self.headers.get("Authorization") != expected:
            self.send_response(401)
            self.send_header("WWW-Authenticate", 'Basic realm="maven-fixture"')
            self.end_headers()
            return
        super().do_GET()


handler = functools.partial(AuthRepository, directory=sys.argv[1])
http.server.ThreadingHTTPServer(("127.0.0.1", 18080), handler).serve_forever()

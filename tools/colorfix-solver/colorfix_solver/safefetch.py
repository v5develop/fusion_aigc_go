"""Outbound fetch for caller-supplied URLs, mirroring our 'safe fetch' practice.

* only http / https, no userinfo;
* host resolved once, every address must be public (loopback / private / link-local / reserved /
  multicast rejected) unless COLORFIX_ALLOW_PRIVATE_FETCH; the socket then connects to that vetted IP
  (no DNS-rebinding window), TLS still validates against the original host name;
* redirects are NOT followed (a 3xx is an error: the redirect target would escape validation);
* hard timeout and hard byte cap, streamed.
"""
from __future__ import annotations

import http.client
import ipaddress
import socket
import ssl
import time
from urllib.parse import urlsplit


class FetchError(Exception):
    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code


def _is_public(ip: ipaddress._BaseAddress) -> bool:
    if isinstance(ip, ipaddress.IPv6Address) and ip.ipv4_mapped is not None:
        ip = ip.ipv4_mapped
    return not (ip.is_private or ip.is_loopback or ip.is_link_local or ip.is_multicast
                or ip.is_reserved or ip.is_unspecified or not ip.is_global)


def resolve_checked(host: str, port: int, *, allow_private: bool) -> str:
    try:
        infos = socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)
    except socket.gaierror as e:
        raise FetchError("fetch_dns", f"cannot resolve host '{host}'") from e
    addrs = []
    for fam, _, _, _, sa in infos:
        if fam in (socket.AF_INET, socket.AF_INET6):
            addrs.append(sa[0].split("%")[0])
    if not addrs:
        raise FetchError("fetch_dns", f"no address for host '{host}'")
    for a in addrs:  # every record must be acceptable, not just the one we pick
        if not allow_private and not _is_public(ipaddress.ip_address(a)):
            raise FetchError("fetch_forbidden", "target address is not public")
    return addrs[0]


def validate_url(url: str, *, allow_hosts: tuple = ()):
    if not isinstance(url, str) or len(url) > 4096:
        raise FetchError("fetch_url", "invalid url")
    try:
        u = urlsplit(url)
        port = u.port
    except ValueError as e:
        raise FetchError("fetch_url", "invalid url") from e
    if u.scheme not in ("http", "https"):
        raise FetchError("fetch_scheme", "only http/https urls are allowed")
    if not u.hostname or u.username or u.password:
        raise FetchError("fetch_url", "invalid url")
    host = u.hostname.lower()
    if allow_hosts and host not in allow_hosts:
        raise FetchError("fetch_forbidden", "host is not in the allow-list")
    return u, host, port or (443 if u.scheme == "https" else 80)


class _PinnedHTTPConnection(http.client.HTTPConnection):
    def __init__(self, host, port, ip, timeout):
        super().__init__(host, port, timeout=timeout)
        self._ip = ip

    def connect(self):
        self.sock = socket.create_connection((self._ip, self.port), self.timeout)


class _PinnedHTTPSConnection(http.client.HTTPSConnection):
    def __init__(self, host, port, ip, timeout):
        super().__init__(host, port, timeout=timeout, context=ssl.create_default_context())
        self._ip = ip

    def connect(self):
        sock = socket.create_connection((self._ip, self.port), self.timeout)
        self.sock = self._context.wrap_socket(sock, server_hostname=self.host)


def fetch(url: str, *, max_bytes: int, timeout: float, allow_private: bool = False, allow_hosts: tuple = ()) -> bytes:
    u, host, port = validate_url(url, allow_hosts=allow_hosts)
    ip = resolve_checked(host, port, allow_private=allow_private)
    cls = _PinnedHTTPSConnection if u.scheme == "https" else _PinnedHTTPConnection
    conn = cls(host, port, ip, timeout)
    deadline = time.monotonic() + timeout
    try:
        path = (u.path or "/") + (f"?{u.query}" if u.query else "")
        conn.request("GET", path, headers={"Host": u.netloc.rsplit("@", 1)[-1], "User-Agent": "colorfix-solver",
                                           "Accept": "image/*", "Connection": "close"})
        resp = conn.getresponse()
        if resp.status != 200:
            raise FetchError("fetch_status", f"upstream returned HTTP {resp.status}")
        cl = resp.getheader("Content-Length")
        if cl and cl.isdigit() and int(cl) > max_bytes:
            raise FetchError("fetch_too_large", "image is too large")
        buf = bytearray()
        while True:
            chunk = resp.read(65536)
            if not chunk:
                break
            buf += chunk
            if len(buf) > max_bytes:
                raise FetchError("fetch_too_large", "image is too large")
            if time.monotonic() > deadline:
                raise FetchError("fetch_timeout", "fetch timed out")
        return bytes(buf)
    except FetchError:
        raise
    except (OSError, http.client.HTTPException, ssl.SSLError) as e:
        raise FetchError("fetch_failed", f"fetch failed: {type(e).__name__}") from e
    finally:
        conn.close()

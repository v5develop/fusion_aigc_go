"""Environment-driven configuration (read once at startup)."""
from __future__ import annotations

import os
from dataclasses import dataclass, field


def _int(name: str, default: int) -> int:
    return int(os.environ.get(name, default))


def _float(name: str, default: float) -> float:
    return float(os.environ.get(name, default))


def _bool(name: str) -> bool:
    return os.environ.get(name, "").strip().lower() in ("1", "true", "yes", "on")


@dataclass(frozen=True)
class Config:
    host: str = "127.0.0.1"
    port: int = 18090
    env: str = "prod"                      # COLORFIX_ENV: "dev" allows running without a token (loopback only)
    token: str = ""                        # COLORFIX_TOKEN: shared bearer token
    max_body: int = 24 * 1024 * 1024       # request body bytes (JSON incl. base64)
    max_image_bytes: int = 12 * 1024 * 1024
    max_pixels: int = 36_000_000           # per decoded image
    max_pieces: int = 8
    max_side: int = 1024                   # solve resolution (long side)
    fetch_timeout: float = 10.0
    solve_timeout: float = 20.0
    read_timeout: float = 15.0             # socket timeout while reading a request (slowloris)
    max_concurrent: int = 4
    allow_private_fetch: bool = False      # COLORFIX_ALLOW_PRIVATE_FETCH
    fetch_allow_hosts: tuple = field(default_factory=tuple)  # COLORFIX_FETCH_ALLOW_HOSTS (optional allow-list)

    @staticmethod
    def from_env() -> "Config":
        hosts = tuple(h.strip().lower() for h in os.environ.get("COLORFIX_FETCH_ALLOW_HOSTS", "").split(",") if h.strip())
        return Config(
            host=os.environ.get("HOST", "127.0.0.1"),
            port=_int("PORT", 18090),
            env=os.environ.get("COLORFIX_ENV", "prod").strip().lower(),
            token=os.environ.get("COLORFIX_TOKEN", ""),
            max_body=_int("COLORFIX_MAX_BODY", 24 * 1024 * 1024),
            max_image_bytes=_int("COLORFIX_MAX_IMAGE_BYTES", 12 * 1024 * 1024),
            max_pixels=_int("COLORFIX_MAX_PIXELS", 36_000_000),
            max_pieces=_int("COLORFIX_MAX_PIECES", 8),
            max_side=_int("COLORFIX_SOLVE_MAX_SIDE", 1024),
            fetch_timeout=_float("COLORFIX_FETCH_TIMEOUT", 10.0),
            solve_timeout=_float("COLORFIX_SOLVE_TIMEOUT", 20.0),
            read_timeout=_float("COLORFIX_READ_TIMEOUT", 15.0),
            max_concurrent=_int("COLORFIX_MAX_CONCURRENT", 4),
            allow_private_fetch=_bool("COLORFIX_ALLOW_PRIVATE_FETCH"),
            fetch_allow_hosts=hosts,
        )

    def validate(self) -> None:
        """Refuse unsafe start-up combinations."""
        if not self.token:
            if self.env != "dev":
                raise SystemExit("COLORFIX_TOKEN is required (set COLORFIX_ENV=dev to run unauthenticated on loopback)")
            if self.host not in ("127.0.0.1", "::1", "localhost"):
                raise SystemExit("unauthenticated dev mode is only allowed on a loopback HOST")
        elif len(self.token) < 16:
            raise SystemExit("COLORFIX_TOKEN must be at least 16 characters")

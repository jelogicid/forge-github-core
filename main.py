# github-core-clone-01 entry launcher.
# The pod runs `python3 -P -s /bot/main.py`. The forge is a statically linked
# Linux amd64 binary committed beside this file as vendored source; this
# launcher hands the process over to it. The binary binds an ephemeral port on
# 0.0.0.0, registers with the env, serves git smart HTTP + REST v3 + GraphQL +
# MCP from its own store, and exits 0 when the env says the trial is done.
import os
import sys

BINARY_NAME = "forge"


def binary_path() -> str:
    here = os.path.dirname(os.path.realpath(__file__))
    return os.path.join(here, BINARY_NAME)


def hand_over(binary: str) -> None:
    # /bot is read-only; the binary is committed with the exec bit set already.
    try:
        os.chmod(binary, 0o755)
    except OSError:
        pass
    argv = [binary] + sys.argv[1:]
    os.execv(binary, argv)


def main() -> None:
    binary = binary_path()
    if not os.path.isfile(binary):
        sys.stderr.write("[entry] vendored binary missing: %s\n" % binary)
        sys.exit(1)
    try:
        hand_over(binary)
    except OSError as exc:
        sys.stderr.write("[entry] cannot exec %s: %s\n" % (binary, exc))
        sys.exit(1)


if __name__ == "__main__":
    main()
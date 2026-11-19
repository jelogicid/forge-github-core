# github-core-clone-01 entry launcher.
# The pod runs `python3 -P -s /bot/main.py`; the forge itself is a statically
# linked Linux amd64 binary committed beside this file as vendored source.
# This launcher only hands the process over to that binary, which binds an
# ephemeral port, registers with the env, serves the four surfaces and polls /done.
import os
import sys


def main() -> None:
    here = os.path.dirname(os.path.abspath(__file__))
    binary = os.path.join(here, "forge")
    # /bot is read-only; the binary is committed with the exec bit already set.
    try:
        os.chmod(binary, 0o755)
    except OSError:
        pass
    os.execv(binary, [binary])


if __name__ == "__main__":
    main()
"""Optional PolyScript startup hook for python3-polyscript."""

import os

if os.environ.get("POLYSCRIPT_AUTO_IMPORT") == "1":
    import polyscript

    polyscript.install()

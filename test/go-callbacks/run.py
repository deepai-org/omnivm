"""Run a manifest with python+javascript initialized (callbacks passed to Go
funcs aren't always detected by manifest scanning, so init explicitly)."""
import sys
sys.path.insert(0, "/build/pyomnivm")
import omnivm
omnivm.init_runtimes(["javascript"])
try:
    omnivm.run_manifest(sys.argv[1])
finally:
    omnivm.shutdown()

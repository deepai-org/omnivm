import sys
import json

data = {"language": "Python", "version": sys.version}
print(json.dumps(data, indent=2))

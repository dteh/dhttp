#!/usr/bin/env python3
"""Forward rewrite: vanilla net/http imports -> dhttp module-path imports.

Applied to a scratch tree mid-build, after patches are applied but before
the tree is copied into the repo. The reverse of the one-shot
reverse_rewrite.py used during the initial patch-series migration.

Leaves utls imports alone (they come from patches, not from this rewrite).
"""
import os
import re
import sys

TREE = sys.argv[1]

REWRITES = [
    # net/http subpackages (use word-boundary-safe quoted-string match).
    (re.compile(r'"net/http/(cgi|cookiejar|fcgi|httptest|httptrace|httputil|pprof)"'),
     r'"github.com/dteh/dhttp/\1"'),
    # net/http internal packages (must be matched before the bare "net/http" rewrite).
    (re.compile(r'"net/http/internal/(ascii|testcert)"'),
     r'"github.com/dteh/dhttp/internal/\1"'),
    (re.compile(r'"net/http/internal"'),
     r'"github.com/dteh/dhttp/internal"'),
    # src/internal/* packages — they live under dhttp/internal/.
    (re.compile(r'"internal/(bisect|cfg|diff|goarch|godebug|godebugs|nettrace|platform|profile|race|synctest|testenv|txtar)"'),
     r'"github.com/dteh/dhttp/internal/\1"'),
    # Bare "net/http" -> named alias (test files use `. "net/http"` separately).
    (re.compile(r'^(\s*)"net/http"$', re.M),
     r'\1http "github.com/dteh/dhttp"'),
    (re.compile(r'^(\s*)\. "net/http"$', re.M),
     r'\1. "github.com/dteh/dhttp"'),
]

def rewrite_line(line: str) -> str:
    """Apply rewrites to a single line, but only if it looks like an import line.

    Comments and string literals containing the same quoted package paths are
    left alone. Go import syntax is always one of:
        import "X"
        import alias "X"
        \tindented "X" (inside `import ( ... )` block)
        \tindented alias "X"
        \tindented . "X" / _ "X"
    A `//` anywhere before the match means the line is a comment; skip it.
    """
    # Quick reject for comment lines.
    stripped = line.lstrip()
    if stripped.startswith('//') or stripped.startswith('/*') or stripped.startswith('*'):
        return line
    # Only rewrite lines that look like import statements.
    # An import line either starts with `import ` or is just whitespace + quoted/aliased import.
    if not (stripped.startswith('import ') or
            stripped.startswith('"') or
            stripped.startswith('. "') or
            stripped.startswith('_ "') or
            (len(stripped) > 0 and stripped[0].isalpha() and ' "' in stripped[:60])):
        return line
    new = line
    for pat, repl in REWRITES:
        new = pat.sub(repl, new)
    return new

changed = 0
for root, _, files in os.walk(TREE):
    for name in files:
        if not name.endswith('.go'):
            continue
        path = os.path.join(root, name)
        with open(path, encoding='utf-8') as f:
            content = f.read()
        new_lines = [rewrite_line(line) for line in content.splitlines(keepends=True)]
        new = ''.join(new_lines)
        if new != content:
            with open(path, 'w', encoding='utf-8') as f:
                f.write(new)
            changed += 1

print(f"Forward-rewrote {changed} files")

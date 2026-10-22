"""Split Go test files into top-level declarations.

decls(text) -> (header, imports(list of import lines), [(name, text)])
Methods are named Type.method; a declaration keeps the comment lines above it.
"""
import re, sys


def decls(text):
    lines = text.split('\n')
    i = 0
    while not lines[i].startswith('import'):
        i += 1
    if lines[i].endswith('('):
        j = i
        while lines[j] != ')':
            j += 1
        imports = lines[i + 1:j]
        end = j + 1
    else:
        imports = [lines[i][len('import '):]]
        end = i + 1
    header = '\n'.join(lines[:i])
    body = lines[end:]
    out = []
    k = 0
    pending = []
    while k < len(body):
        l = body[k]
        if l.startswith('//') or l == '':
            pending.append(l)
            k += 1
            continue
        m = re.match(r'^(func|type|var|const)\s*(\([^)]*\)\s*)?(\w+)?', l)
        if m and not l.startswith(' ') and not l.startswith('\t'):
            kind = m.group(1)
            if kind == 'func' and m.group(2):
                recv = re.search(r'\*?(\w+)\)', m.group(2)).group(1)
                nm = recv + '.' + re.match(r'^func\s*\([^)]*\)\s*(\w+)', l).group(1)
            else:
                nm = m.group(3) or ('%s@%d' % (kind, k))
            blk = list(pending)
            pending = []
            if l.rstrip().endswith('{') or l.rstrip().endswith('('):
                closer = '}' if l.rstrip().endswith('{') else ')'
                while True:
                    blk.append(body[k])
                    if body[k] == closer:
                        break
                    k += 1
            else:
                blk.append(l)
            k += 1
            out.append((nm, '\n'.join(blk).strip('\n')))
        else:
            raise SystemExit('unparsed line %d: %r' % (k, l))
    return header, imports, out


if __name__ == '__main__':
    for f in sys.argv[1:]:
        h, imps, ds = decls(open(f, encoding='utf-8').read())
        print(f, len(ds), [d[0] for d in ds])

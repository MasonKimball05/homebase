"""Mutants of one top-level Python function, for Away's "do these tests test it?" check (mutants.go).

    python mutants.py list FILE FUNCTION          JSON: [{"id": 0, "kind": ..., "line": ..., "what": ...}, ...]
    python mutants.py apply FILE FUNCTION ID      rewrites FILE with mutant ID (Away restores it afterwards)

Mutant 0 is always the stub: the whole function, decorators and all, replaced
by one that raises. A test that still passes never really calls it. The rest
change one thing each: a comparison, a `not`, an `and`/`or`, an `if` turned
around, a returned constant (or a True/False in a returned tuple), one link
of a query chain dropped (`.filter(...)`, `.exclude(...)`, `.order_by(...)`),
or one decorator taken away (a test of @login_required or
@require_POST should notice). Only the function's own lines change; the rest
of the file is kept byte for byte.
"""
import ast
import copy
import json
import re
import sys

MAX = 9  # the stub plus 8 others: each one is a test run

# Decorators that guard a view. Taking away a logging or caching one changes
# nothing a test should notice, so only these are removed.
GATE = re.compile(r"login|required|require_|permission|admin|officer|staff|csrf|auth|cnb", re.I)

# Query-chain links whose absence a test of the result should notice (not
# select_related and the like, which only change how many queries it takes).
# Decorators that check who you are, and so the login too.
WHO = re.compile(r"officer|admin|permission|staff|cnb|chair", re.I)

CHAIN = {"filter", "exclude", "order_by", "distinct", "first", "last"}

FLIP = {ast.Lt: ast.LtE, ast.LtE: ast.Lt, ast.Gt: ast.GtE, ast.GtE: ast.Gt, ast.Eq: ast.NotEq, ast.NotEq: ast.Eq,
        ast.In: ast.NotIn, ast.NotIn: ast.In, ast.Is: ast.IsNot, ast.IsNot: ast.Is}
SYMBOL = {ast.Lt: "<", ast.LtE: "<=", ast.Gt: ">", ast.GtE: ">=", ast.Eq: "==", ast.NotEq: "!=",
          ast.In: "in", ast.NotIn: "not in", ast.Is: "is", ast.IsNot: "is not"}


def load(path, name):
    # newline="": keep the file's own line endings (the PC's are CRLF).
    src = open(path, encoding="utf-8", newline="").read()
    tree = ast.parse(src)
    for node in tree.body:
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name == name:
            return src, node
    raise SystemExit(f"no top-level function {name} in {path}")


def own_nodes(fn):
    """The function's nodes in source order, not those of functions or lambdas nested in it."""
    out = []

    def walk(n):
        for child in ast.iter_child_nodes(n):
            if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda, ast.ClassDef)):
                continue
            out.append(child)
            walk(child)

    for stmt in fn.body:
        out.append(stmt)
        walk(stmt)
    return out


def text(node):
    return ast.unparse(node).replace("\n", " ")[:80]


def candidates(fn):
    """(kind, line, what, change) for every mutant; change(copy_of_fn) applies it."""
    found = [("stub", fn.lineno, f"{fn.name} replaced by a function that only raises an error", None)]
    nodes = own_nodes(fn)
    for i, n in enumerate(nodes):
        if isinstance(n, ast.Compare):
            for j, op in enumerate(n.ops):
                if type(op) in FLIP:
                    new = FLIP[type(op)]
                    found.append(("compare", n.lineno, f"`{text(n)}` with {SYMBOL[type(op)]} changed to {SYMBOL[new]}",
                                  (i, lambda m, j=j, new=new: m.ops.__setitem__(j, new()))))
        elif isinstance(n, ast.UnaryOp) and isinstance(n.op, ast.Not):
            found.append(("not", n.lineno, f"`{text(n)}` without the `not`", (i, "drop-not")))
        elif isinstance(n, ast.BoolOp):
            other = ast.Or if isinstance(n.op, ast.And) else ast.And
            found.append(("boolop", n.lineno, f"`{text(n)}` with {'and' if other is ast.Or else 'or'} changed to {'or' if other is ast.Or else 'and'}",
                          (i, lambda m, other=other: setattr(m, "op", other()))))
        elif isinstance(n, ast.Call) and isinstance(n.func, ast.Attribute) and n.func.attr in CHAIN:
            link = copy.copy(n)
            link.func = ast.Attribute(value=ast.Name(id="_", ctx=ast.Load()), attr=n.func.attr, ctx=ast.Load())
            found.append(("chain", n.lineno, f"`{text(link)[1:]}` left out of the query", (i, "drop-call")))
        elif isinstance(n, ast.If) and not (isinstance(n.test, ast.UnaryOp) and isinstance(n.test.op, ast.Not)):
            found.append(("if", n.lineno, f"`if {text(n.test)}` turned around (`if not (...)`)",
                          (i, lambda m: setattr(m, "test", ast.UnaryOp(op=ast.Not(), operand=m.test)))))
        elif isinstance(n, ast.Return) and isinstance(n.value, ast.Tuple):
            for j, e in enumerate(n.value.elts):
                if isinstance(e, ast.Constant) and isinstance(e.value, bool):
                    found.append(("return", n.lineno, f"`return {text(n.value)}` with {e.value!r} changed to {not e.value!r}",
                                  (i, lambda m, j=j: setattr(m.value.elts[j], "value", not m.value.elts[j].value))))
                    break
        elif isinstance(n, ast.Return) and isinstance(n.value, ast.Constant):
            v = n.value.value
            if isinstance(v, bool):
                found.append(("return", n.lineno, f"`return {v!r}` changed to `return {not v!r}`", (i, lambda m: setattr(m.value, "value", not m.value.value))))
            elif isinstance(v, (int, float)):
                found.append(("return", n.lineno, f"`return {v!r}` changed to `return {v + 1!r}`", (i, lambda m: setattr(m.value, "value", m.value.value + 1))))
            elif isinstance(v, str) and v:
                found.append(("return", n.lineno, f"`return {v!r}` changed to `return ''`", (i, lambda m: setattr(m.value, "value", ""))))
    for k, d in enumerate(fn.decorator_list):
        if not GATE.search(text(d)):
            continue
        # @officer_required and its kind check the login themselves, so taking
        # @login_required away from beside them changes nothing.
        if "login_required" in text(d) and any(WHO.search(text(o)) for o in fn.decorator_list):
            continue
        found.append(("decorator", d.lineno, f"the @{text(d)} decorator removed", ("decorator", k)))
    # Decorators are the cheapest to get wrong in a view and the most important
    # to test, so they go first; then the rest in source order.
    found = found[:1] + [f for f in found[1:] if f[0] == "decorator"] + [f for f in found[1:] if f[0] != "decorator"]
    return found[:MAX]


def mutate(fn, change):
    m = copy.deepcopy(fn)
    if change is None:
        return ast.parse(f"def {fn.name}(*args, **kwargs):\n    raise RuntimeError('Away mutant: {fn.name} replaced')\n").body[0]
    where, how = change
    if where == "decorator":
        del m.decorator_list[how]
        return m
    target = own_nodes(m)[where]
    if how in ("drop-not", "drop-call"):
        replacement = target.operand if how == "drop-not" else target.func.value
        parent = next(p for p in ast.walk(m) for f, v in ast.iter_fields(p)
                      if v is target or (isinstance(v, list) and any(x is target for x in v)))
        for f, v in ast.iter_fields(parent):
            if v is target:
                setattr(parent, f, replacement)
            elif isinstance(v, list):
                for idx, x in enumerate(v):
                    if x is target:
                        v[idx] = replacement
    else:
        how(target)
    return m


def main():
    cmd, path, name = sys.argv[1:4]
    src, fn = load(path, name)
    found = candidates(fn)
    if cmd == "list":
        print(json.dumps([{"id": i, "kind": k, "line": line, "what": what} for i, (k, line, what, _) in enumerate(found)]))
        return
    i = int(sys.argv[4])
    new = ast.fix_missing_locations(mutate(fn, found[i][3]))
    lines = src.splitlines(keepends=True)
    start = min([fn.lineno] + [d.lineno for d in fn.decorator_list]) - 1
    nl = "\r\n" if "\r\n" in src else "\n"
    out = "".join(lines[:start]) + ast.unparse(new).replace("\n", nl) + nl + "".join(lines[fn.end_lineno:])
    compile(out, path, "exec")  # never leave a file that doesn't parse
    open(path, "w", encoding="utf-8", newline="").write(out)


if __name__ == "__main__":
    main()

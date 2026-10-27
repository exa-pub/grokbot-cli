#!/usr/bin/env python3
"""Reconstruct .proto schema files from protobuf-es style minified JS bundles.

Static analysis only: the bundle is never executed.

Supported encodings found in the bundles:
  * Compact message descriptors:
        X=class t extends BASE{... static $(){return["Name|1 f 9|2 g #0?|3 h 9,#1|4 k #2 oneofname", A, B, C]}}
    - first segment: message name relative to package (nested: "Outer.Inner")
    - each "|"-separated field: "<no> <name> <type>[?|*] [oneof]"
        type   = decimal ScalarType code (protobuf-es / descriptor.proto numbering)
               | "#N"  -> N-th extra array element (class => message, enum object => enum)
               | "K,V" -> map<K, V> (K scalar code, V scalar code or #N)
        suffix "?" -> proto3 optional, "*" -> repeated; 4th token -> oneof name
    - package comes from BASE's `static $p(){return PKGVAR}` where `var PKGVAR="agent.v1."`
  * Compact enum descriptors:
        X=HELPER(RUNTIME,PKGVAR,"Name",[[0,"A"],[1,"B"]],FLAG)
    FLAG===1  -> proto value names are UPPER_SNAKE(Name local part)+"_"+given name
    otherwise -> proto value names are the given names verbatim
  * Classic protobuf-es:  X.typeName="pkg.Name";X.fields=RT.util.newFieldList(()=>[{no:..},..])
    and RT.util.setEnumType(X,"pkg.Name",[{no:0,name:"A"},..])
  * Services: {typeName:"pkg.FooService",methods:{m:{name:"M",I:Req,O:Resp,kind:RT.Unary},..}}

Usage:
  extract-proto.py OUT_DIR BUNDLE [BUNDLE ...]
      [--subset FILE.proto:SERVICE:Method1,Method2,...]   (repeatable)
      [--go-package-prefix example.com/mod/gen]
"""
import argparse
import bisect
import json
import os
import re
import sys
from collections import defaultdict, OrderedDict

SCALAR = {1: "double", 2: "float", 3: "int64", 4: "uint64", 5: "int32", 6: "fixed64",
          7: "fixed32", 8: "bool", 9: "string", 12: "bytes", 13: "uint32",
          15: "sfixed32", 16: "sfixed64", 17: "sint32", 18: "sint64"}

ID = r"[A-Za-z_$][\w$]*"

WKT_FILES = {
    "google.protobuf.Struct": "google/protobuf/struct.proto",
    "google.protobuf.Value": "google/protobuf/struct.proto",
    "google.protobuf.ListValue": "google/protobuf/struct.proto",
    "google.protobuf.NullValue": "google/protobuf/struct.proto",
    "google.protobuf.Timestamp": "google/protobuf/timestamp.proto",
    "google.protobuf.Empty": "google/protobuf/empty.proto",
    "google.protobuf.Duration": "google/protobuf/duration.proto",
    "google.protobuf.Any": "google/protobuf/any.proto",
    "google.protobuf.FieldMask": "google/protobuf/field_mask.proto",
}
for _w in ("Bool", "String", "Int32", "Int64", "UInt32", "UInt64", "Double", "Float", "Bytes"):
    WKT_FILES["google.protobuf.%sValue" % _w] = "google/protobuf/wrappers.proto"


# --------------------------------------------------------------------------- JS literal mini-parser
class JSLit:
    """Parses a subset of JS literals: arrays, objects, strings, numbers, identifiers,
    !0/!1, member expressions and simple calls (returned as ('call', callee, args))."""

    def __init__(self, s, i):
        self.s, self.i = s, i

    def ws(self):
        s = self.s
        while self.i < len(s) and s[self.i] in " \t\r\n":
            self.i += 1

    def peek(self):
        self.ws()
        return self.s[self.i]

    def expect(self, ch):
        self.ws()
        if self.s[self.i] != ch:
            raise ValueError("expected %r at %d got %r" % (ch, self.i, self.s[self.i:self.i + 40]))
        self.i += 1

    def value(self):
        self.ws()
        c = self.s[self.i]
        if c == "[":
            self.i += 1
            out = []
            while self.peek() != "]":
                out.append(self.value())
                if self.peek() == ",":
                    self.i += 1
            self.i += 1
            return out
        if c == "{":
            self.i += 1
            out = OrderedDict()
            while self.peek() != "}":
                m = re.compile(r'"([^"]*)"|(' + ID + ')').match(self.s, self.i)
                key = m.group(1) if m.group(1) is not None else m.group(2)
                self.i = m.end()
                self.expect(":")
                out[key] = self.value()
                if self.peek() == ",":
                    self.i += 1
            self.i += 1
            return out
        if c in "\"'`":
            j = self.i + 1
            buf = []
            while self.s[j] != c:
                if self.s[j] == "\\":
                    buf.append(self.s[j:j + 2])
                    j += 2
                else:
                    buf.append(self.s[j])
                    j += 1
            self.i = j + 1
            return json.loads('"' + "".join(buf).replace('"', '\\"').replace("\\'", "'") + '"')
        if c == "!":
            self.i += 2
            return self.s[self.i - 1] == "0"
        m = re.compile(r"-?\d+(\.\d+)?(e[+-]?\d+)?").match(self.s, self.i)
        if m:
            self.i = m.end()
            return float(m.group(0)) if (m.group(1) or m.group(2)) else int(m.group(0))
        m = re.compile(ID + r"(\." + ID + r")*").match(self.s, self.i)
        if m:
            self.i = m.end()
            name = m.group(0)
            if self.i < len(self.s) and self.s[self.i] == "(":
                self.i += 1
                args = []
                while self.peek() != ")":
                    args.append(self.value())
                    if self.peek() == ",":
                        self.i += 1
                self.i += 1
                return ("call", name, args)
            if name == "void":
                self.ws()
                self.value()
                return None
            return ("id", name)
        raise ValueError("cannot parse at %d: %r" % (self.i, self.s[self.i:self.i + 60]))


def parse_at(s, i):
    p = JSLit(s, i)
    return p.value(), p.i


# --------------------------------------------------------------------------- model
class Field:
    def __init__(self, no, name, label="", typ=None, oneof=None, key=None):
        self.no, self.name, self.label, self.typ, self.oneof, self.key = no, name, label, typ, oneof, key
        # typ: ("scalar", code) | ("ref", js_ident) | ("full", "pkg.Name") ; key: scalar code for maps

    def sig(self):
        return (self.no, self.name, self.label, self.oneof, self.key, self.typ)


class Msg:
    def __init__(self, full, fields, bundle):
        self.full, self.fields, self.bundle = full, fields, bundle


class Enum:
    def __init__(self, full, values, bundle):
        self.full, self.values, self.bundle = full, values, bundle


class Service:
    def __init__(self, full, methods, bundle):
        self.full, self.methods, self.bundle = full, methods, bundle  # methods: [(name, in, out, kind)]


class Schema:
    def __init__(self):
        self.msgs, self.enums, self.services = {}, {}, {}
        self.warnings = []

    def add(self, table, key, obj, sigf):
        old = table.get(key)
        if old is None:
            table[key] = obj
        elif sigf(old) != sigf(obj):
            self.warnings.append("conflicting definitions of %s in %s and %s (keeping first)"
                                 % (key, old.bundle, obj.bundle))


# --------------------------------------------------------------------------- bundle extraction
def extract_bundle(path, schema):
    src = open(path, "r", encoding="utf-8", errors="replace").read()
    bname = os.path.basename(os.path.dirname(path)) + "/" + os.path.basename(path)

    # package string vars
    pkgvars = {}
    for m in re.finditer(r"(?:var |let |const |,)(" + ID + r')="((?:[a-z_][a-z0-9_]*\.)+)"', src):
        pkgvars[m.group(1)] = m.group(2)

    # class headers
    hdr_re = re.compile(r"(?<![\w$.])(" + ID + r")=class(?: (" + ID + r"))? extends (" + ID + r")\{")
    headers = [(m.start(), m.group(1), m.group(3), m.group(2)) for m in hdr_re.finditer(src)]
    hpos = [h[0] for h in headers]
    extends = {}
    for _, n, b, _i in headers:
        extends.setdefault(n, b)

    def class_at(pos):
        k = bisect.bisect_right(hpos, pos) - 1
        return headers[k] if k >= 0 else None

    # base classes carrying the package
    base_pkg = {}
    for m in re.finditer(r"static \$p\(\)\{return (" + ID + r")\}", src):
        h = class_at(m.start())
        if h and m.group(1) in pkgvars:
            base_pkg[h[1]] = pkgvars[m.group(1)]

    def pkg_of(cls):
        seen = 0
        while cls is not None and seen < 20:
            if cls in base_pkg:
                return base_pkg[cls]
            cls = extends.get(cls)
            seen += 1
        return None

    ident_kind = {}   # js ident -> ("msg"|"enum", fullname)
    pending = []      # (Msg, raw_compact_fields, args)

    # --- compact enums: X=HELPER(RT,PKGVAR,"Name",[[..]],FLAG)
    enum_re = re.compile(r"(?<![\w$.])(" + ID + r")=(" + ID + r")\((" + ID + r"),(" + ID + r'),"([\w.]+)",\[\[')
    for m in enum_re.finditer(src):
        if m.group(4) not in pkgvars:
            continue
        pkg = pkgvars[m.group(4)]
        rel = m.group(5)
        pairs, j = parse_at(src, m.end() - 2)
        flag = None
        p = JSLit(src, j)
        if p.peek() == ",":
            p.i += 1
            flag = p.value()
        local = rel.rsplit(".", 1)[-1]
        if flag == 1:
            pref = (local[0] + re.sub(r"[A-Z]", lambda x: "_" + x.group(0), local[1:])).upper() + "_"
            values = [(pref + nm, no) for no, nm in pairs]
        else:
            values = [(nm, no) for no, nm in pairs]
        full = pkg + rel
        schema.add(schema.enums, full, Enum(full, values, bname), lambda e: e.values)
        ident_kind[m.group(1)] = ("enum", full)

    # --- classic enums: setEnumType(X,"full",[{no,name}])
    for m in re.finditer(r"setEnumType\((" + ID + r'),"([\w.]+)",\[', src):
        arr, _ = parse_at(src, m.end() - 1)
        values = [(o["name"], o["no"]) for o in arr]
        schema.add(schema.enums, m.group(2), Enum(m.group(2), values, bname), lambda e: e.values)
        ident_kind[m.group(1)] = ("enum", m.group(2))

    # --- compact messages
    for m in re.finditer(r"static \$\(\)\{return\[", src):
        arr, _ = parse_at(src, m.end() - 1)
        h = class_at(m.start())
        if not h:
            schema.warnings.append("%s: compact descriptor without class header at %d" % (bname, m.start()))
            continue
        pkg = pkg_of(h[2])
        if pkg is None:
            schema.warnings.append("%s: no package for class %s (%s)" % (bname, h[1], arr[0][:40]))
            continue
        desc = arr[0]
        args = [a[1] if isinstance(a, tuple) and a[0] == "id" else a for a in arr[1:]]
        # `class t extends ...{... $(){return["X|1 child #0*", t]}}` -> self reference via inner class name
        args = [h[1] if (h[3] and a == h[3]) else a for a in args]
        segs = desc.split("|")
        full = pkg + segs[0]
        fields = []
        for seg in segs[1:]:
            parts = seg.split(" ")
            no, name, t = int(parts[0]), parts[1], parts[2]
            mod = parts[3] if len(parts) > 3 else None
            if t[-1] in "?*":
                mod, t = t[-1], t[:-1]
            label, oneof = "", None
            if mod == "?":
                label = "optional"
            elif mod == "*":
                label = "repeated"
            elif mod:
                oneof = mod

            def conv(tok):
                if tok.startswith("#"):
                    return ("ref", args[int(tok[1:])])
                return ("scalar", int(tok))
            if "," in t:
                k, v = t.split(",", 1)
                fields.append(Field(no, name, "map", conv(v), None, int(k)))
            else:
                fields.append(Field(no, name, label, conv(t), oneof))
        msg = Msg(full, fields, bname)
        ident_kind[h[1]] = ("msg", full)
        pending.append(msg)

    # --- classic messages
    for m in re.finditer(r"(?<![\w$.])(" + ID + r')\.typeName="([\w.]+)";\1\.fields=' + ID +
                         r"\.util\.newFieldList\(\(\)=>\[", src):
        arr, _ = parse_at(src, m.end() - 1)
        ident_kind[m.group(1)] = ("msg", m.group(2))
        fields = []
        for o in arr:
            def conv(kind, T):
                if kind == "scalar":
                    return ("scalar", T)
                if isinstance(T, tuple) and T[0] == "call":   # RT.getEnumType(X)
                    return ("ref", T[2][0][1])
                return ("ref", T[1])
            if o["kind"] == "map":
                V = o["V"]
                fields.append(Field(o["no"], o["name"], "map", conv(V["kind"], V["T"]), None, o["K"]))
            else:
                label = "repeated" if o.get("repeated") else ("optional" if o.get("opt") else "")
                fields.append(Field(o["no"], o["name"], label, conv(o["kind"], o["T"]), o.get("oneof")))
        pending.append(Msg(m.group(2), fields, bname))

    # --- resolve refs
    for msg in pending:
        for f in msg.fields:
            if f.typ[0] == "ref":
                k = ident_kind.get(f.typ[1])
                if k is None:
                    schema.warnings.append("%s: unresolved type ref %s in %s.%s" % (bname, f.typ[1], msg.full, f.name))
                    f.typ = ("full", "google.protobuf.Empty")
                else:
                    f.typ = ("full", k[1])
        schema.add(schema.msgs, msg.full, msg, lambda x: [f.sig() for f in x.fields])

    # --- services
    for m in re.finditer(r'\{typeName:"([\w.]+)",methods:\{', src):
        obj, _ = parse_at(src, m.start())
        methods = []
        for _, md in obj["methods"].items():
            def res(v):
                k = ident_kind.get(v[1])
                if k is None:
                    schema.warnings.append("%s: unresolved method type %s" % (bname, v[1]))
                    return "google.protobuf.Empty"
                return k[1]
            kind = md["kind"][1].rsplit(".", 1)[-1] if isinstance(md["kind"], tuple) else str(md["kind"])
            methods.append((md["name"], res(md["I"]), res(md["O"]), kind))
        schema.add(schema.services, obj["typeName"], Service(obj["typeName"], methods, bname),
                   lambda s: s.methods)
    return len(pending)


# --------------------------------------------------------------------------- emission
def pkg_split(full, schema):
    """Split full name into (package, relative) using known package names."""
    parts = full.split(".")
    i = 0
    while i < len(parts) - 1 and parts[i][:1].islower():
        i += 1
    return ".".join(parts[:i]), ".".join(parts[i:])


def type_name(f):
    if f.typ[0] == "scalar":
        return SCALAR.get(f.typ[1], "UNKNOWN_SCALAR_%s" % f.typ[1])
    return "." + f.typ[1]


def deps_of_msg(msg):
    out = []
    for f in msg.fields:
        if f.typ[0] == "full":
            out.append(f.typ[1])
    return out


def closure(schema, roots):
    seen, stack = set(), list(roots)
    while stack:
        n = stack.pop()
        if n in seen:
            continue
        seen.add(n)
        if n in schema.msgs:
            stack.extend(deps_of_msg(schema.msgs[n]))
            # nested parents must exist for nested names
        parent = n.rsplit(".", 1)[0]
        if parent in schema.msgs:
            stack.append(parent)
    return seen


def render_file(schema, pkg, names, services, go_pkg=None, header="", file_of=None):
    """names: set of full type names (all in pkg); services: list of Service."""
    lines = [header] if header else []
    lines.append('syntax = "proto3";')
    lines.append("")
    lines.append("package %s;" % pkg)
    lines.append("")
    imports = set()
    ext_types = set()
    for n in names:
        if n in schema.msgs:
            for d in deps_of_msg(schema.msgs[n]):
                if not d.startswith(pkg + ".") or (file_of is not None and d not in names):
                    ext_types.add(d)
    for s in services:
        for _, i, o, _k in s.methods:
            for d in (i, o):
                if not d.startswith(pkg + ".") or (file_of is not None and d not in names):
                    ext_types.add(d)
    for t in ext_types:
        if t in WKT_FILES:
            imports.add(WKT_FILES[t])
        elif file_of is not None:
            if file_of.get(t) is not None:
                imports.add(file_of[t])
        else:
            imports.add(pkg_split(t, schema)[0].replace(".", "_") + ".proto")
    for imp in sorted(imports):
        lines.append('import "%s";' % imp)
    if imports:
        lines.append("")
    if go_pkg:
        lines.append('option go_package = "%s";' % go_pkg)
        lines.append("")

    # build nesting tree
    children = defaultdict(list)
    tops = []
    for n in sorted(names):
        rel = n[len(pkg) + 1:]
        if "." in rel:
            parent = pkg + "." + rel.rsplit(".", 1)[0]
            if parent in names:
                children[parent].append(n)
                continue
            schema.warnings.append("nested type %s has no parent in output; emitted at top level" % n)
        tops.append(n)

    def emit(n, ind):
        local = n.rsplit(".", 1)[-1]
        if n not in names or (n in schema.msgs and n in schema.enums):
            pass
        pad = "  " * ind
        if n in schema.enums:
            e = schema.enums[n]
            lines.append("%senum %s {" % (pad, local if "." not in n[len(pkg) + 1:] or n.rsplit('.', 1)[0] in names else n[len(pkg)+1:].replace(".", "_")))
            vals = e.values
            if len({v for _, v in vals}) != len(vals):
                lines.append("%s  option allow_alias = true;" % pad)
            for vn, vno in vals:
                lines.append("%s  %s = %d;" % (pad, vn, vno))
            lines.append("%s}" % pad)
            return
        msg = schema.msgs[n]
        nm = local if "." not in n[len(pkg) + 1:] or n.rsplit('.', 1)[0] in names else n[len(pkg)+1:].replace(".", "_")
        lines.append("%smessage %s {" % (pad, nm))
        for c in children.get(n, []):
            emit(c, ind + 1)
        oneofs = OrderedDict()
        items = []
        for f in msg.fields:
            if f.oneof:
                if f.oneof not in oneofs:
                    oneofs[f.oneof] = []
                    items.append(("oneof", f.oneof))
                oneofs[f.oneof].append(f)
            else:
                items.append(("field", f))
        for kind, it in items:
            if kind == "field":
                f = it
                if f.label == "map":
                    t = "map<%s, %s>" % (SCALAR[f.key], type_name(f))
                    lines.append("%s  %s %s = %d;" % (pad, t, f.name, f.no))
                else:
                    lab = (f.label + " ") if f.label else ""
                    lines.append("%s  %s%s %s = %d;" % (pad, lab, type_name(f), f.name, f.no))
            else:
                lines.append("%s  oneof %s {" % (pad, it))
                for f in oneofs[it]:
                    lines.append("%s    %s %s = %d;" % (pad, type_name(f), f.name, f.no))
                lines.append("%s  }" % pad)
        lines.append("%s}" % pad)

    for n in tops:
        emit(n, 0)
        lines.append("")
    for s in services:
        lines.append("service %s {" % s.full.rsplit(".", 1)[-1])
        for name, i, o, kind in s.methods:
            ss = "stream " if kind == "ServerStreaming" or kind == "BiDiStreaming" else ""
            cs = "stream " if kind == "ClientStreaming" or kind == "BiDiStreaming" else ""
            lines.append("  rpc %s(%s.%s) returns (%s.%s);" % (name, cs, i, ss, o))
        lines.append("}")
        lines.append("")
    return "\n".join(lines).rstrip() + "\n"


# --------------------------------------------------------------------------- validation
def validate(schema, names, services, label):
    errs = []
    for n in names:
        if n in schema.msgs:
            m = schema.msgs[n]
            nos = [f.no for f in m.fields]
            fnames = [f.name for f in m.fields]
            if len(set(nos)) != len(nos):
                errs.append("%s: duplicate field numbers" % n)
            if len(set(fnames)) != len(fnames):
                errs.append("%s: duplicate field names" % n)
            for f in m.fields:
                if f.typ[0] == "full" and f.typ[1] not in names and f.typ[1] not in WKT_FILES:
                    if f.typ[1] not in schema.msgs and f.typ[1] not in schema.enums:
                        errs.append("%s.%s: undefined type %s" % (n, f.name, f.typ[1]))
                if f.typ[0] == "scalar" and f.typ[1] not in SCALAR:
                    errs.append("%s.%s: unknown scalar %s" % (n, f.name, f.typ[1]))
                if f.oneof and f.label:
                    errs.append("%s.%s: label inside oneof" % (n, f.name))
        elif n in schema.enums:
            e = schema.enums[n]
            if not e.values or e.values[0][1] != 0:
                errs.append("%s: first enum value is not 0" % n)
    # enum value names are scoped to the enclosing scope in proto (C++ rules)
    scope_vals = defaultdict(list)
    for n in names:
        if n in schema.enums:
            for vn, _ in schema.enums[n].values:
                scope_vals[(n.rsplit(".", 1)[0], vn)].append(n)
    for (sc, vn), ens in scope_vals.items():
        if len(ens) > 1:
            errs.append("enum value %s defined in multiple enums in scope %s: %s" % (vn, sc, ens))
    for e in errs:
        print("[%s] VALIDATION: %s" % (label, e), file=sys.stderr)
    return errs


# --------------------------------------------------------------------------- main
def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("out_dir")
    ap.add_argument("bundles", nargs="+")
    ap.add_argument("--subset", action="append", default=[],
                    help="FILE.proto:pkg.Service:Method1,Method2  (emit self-contained subset file)")
    ap.add_argument("--go-package-prefix", default=None,
                    help="emit option go_package = PREFIX/<pkg path>;<pkg alias>")
    ap.add_argument("--json", action="store_true", help="also dump schema.json")
    a = ap.parse_args()

    schema = Schema()
    for b in a.bundles:
        n = extract_bundle(b, schema)
        print("%s: %d message descriptors" % (b, n), file=sys.stderr)
    os.makedirs(a.out_dir, exist_ok=True)

    # nested types whose parent message was tree-shaken out of the bundle: synthesize empty parents
    for n in sorted(list(schema.msgs) + list(schema.enums)):
        pkg, rel = pkg_split(n, schema)
        parts = rel.split(".")
        for k in range(1, len(parts)):
            parent = pkg + "." + ".".join(parts[:k])
            if parent not in schema.msgs:
                schema.msgs[parent] = Msg(parent, [], "placeholder")
                schema.warnings.append("synthesized empty placeholder parent message %s" % parent)

    def gopkg(pkg, suffix=""):
        if not a.go_package_prefix:
            return None
        parts = pkg.split(".")
        return "%s/%s%s;%s" % (a.go_package_prefix, "/".join(parts), suffix, "".join(parts[-2:]) if len(parts) > 1 else parts[0])

    # ---- full dump: one file per package
    bypkg = defaultdict(set)
    for n in list(schema.msgs) + list(schema.enums):
        pkg, _ = pkg_split(n, schema)
        if pkg == "google.protobuf":
            continue
        bypkg[pkg].add(n)
    svc_by_pkg = defaultdict(list)
    for s in schema.services.values():
        svc_by_pkg[s.full.rsplit(".", 1)[0]].append(s)
    all_dir = os.path.join(a.out_dir, "all")
    os.makedirs(all_dir, exist_ok=True)
    files = {pkg.replace(".", "_") + ".proto": (pkg, set(names)) for pkg, names in bypkg.items()}
    file_svcs = {pkg.replace(".", "_") + ".proto": svc_by_pkg.get(pkg, []) for pkg in bypkg}

    def refs_of(names, svcs=()):
        out = set()
        for n in names:
            if n in schema.msgs:
                out.update(deps_of_msg(schema.msgs[n]))
        for sv in svcs:
            for _, i, o, _k in sv.methods:
                out.update((i, o))
        return out

    # break import cycles between packages: move the types of file A that file B needs
    # (plus their in-package closure and nested children) into A_shared.proto (same package)
    for _ in range(10):
        file_of = {n: f for f, (_, ns) in files.items() for n in ns}
        cyc = None
        for fa, (pa, na) in files.items():
            da = {file_of.get(t) for t in refs_of(na, file_svcs.get(fa, []))} - {fa, None}
            for fb in da:
                db = {file_of.get(t) for t in refs_of(files[fb][1], file_svcs.get(fb, []))}
                if fa in db:
                    cyc = (fa, fb)
                    break
            if cyc:
                break
        if not cyc:
            break
        done = False
        for fa, fb in (cyc, cyc[::-1]):
            pa, na = files[fa]
            move = {t for t in refs_of(files[fb][1], file_svcs.get(fb, [])) if t in na}
            while True:
                more = set(move)
                for t in move:
                    more.update(x for x in na if x.startswith(t + "."))
                    rel = t[len(pa) + 1:].split(".")
                    for k in range(1, len(rel)):
                        more.add(pa + "." + ".".join(rel[:k]))
                more.update(x for x in refs_of(move) if x in na)
                if more == move:
                    break
                move = more
            if not any(file_of.get(t) == fb for t in refs_of(move)):
                done = True
                break
        if not done:
            schema.warnings.append("cannot break import cycle %s <-> %s" % cyc)
            break
        shared = fa[:-6] + "_shared.proto"
        files[fa] = (pa, na - move)
        files.setdefault(shared, (pa, set()))[1].update(move)
        print("moved %d types from %s to %s to break cycle with %s" % (len(move), fa, shared, fb), file=sys.stderr)

    file_of = {n: f for f, (_, ns) in files.items() for n in ns}
    for fname, (pkg, names) in sorted(files.items()):
        svcs = sorted(file_svcs.get(fname, []), key=lambda s: s.full)
        validate(schema, names, svcs, fname)
        fn = os.path.join(all_dir, fname)
        with open(fn, "w") as fh:
            fh.write(render_file(schema, pkg, names, svcs, gopkg(pkg),
                                 "// Reconstructed from JS bundles by extract_proto.py (best effort).\n",
                                 file_of=file_of))
        print("wrote %s (%d types, %d services)" % (fn, len(names), len(svcs)), file=sys.stderr)

    # ---- subsets
    for spec in a.subset:
        fname, svc, methods = spec.split(":")
        methods = methods.split(",")
        s = schema.services[svc]
        sel = [m for m in s.methods if m[0] in methods]
        missing = set(methods) - {m[0] for m in sel}
        if missing:
            print("WARNING: methods not found in %s: %s" % (svc, sorted(missing)), file=sys.stderr)
        roots = [x for m in sel for x in (m[1], m[2])]
        names = {n for n in closure(schema, roots) if not n.startswith("google.protobuf.")}
        pkg = svc.rsplit(".", 1)[0]
        foreign = sorted(n for n in names if not n.startswith(pkg + "."))
        if foreign:
            # rehome foreign-package types into the subset package so the file stays self-contained
            print("NOTE: subset %s pulls foreign-package types (rehomed): %s" % (fname, foreign), file=sys.stderr)
        sub = Schema()
        rename = {}
        for n in names:
            if not n.startswith(pkg + "."):
                rename[n] = pkg + "." + n.split(".", 2)[-1]
        for n in names:
            nn = rename.get(n, n)
            if n in schema.msgs:
                om = schema.msgs[n]
                fs = []
                for f in om.fields:
                    t = f.typ
                    if t[0] == "full" and t[1] in rename:
                        t = ("full", rename[t[1]])
                    fs.append(Field(f.no, f.name, f.label, t, f.oneof, f.key))
                sub.msgs[nn] = Msg(nn, fs, om.bundle)
            else:
                sub.enums[nn] = schema.enums[n]
        sub_svc = Service(svc, sel, s.bundle)
        validate(sub, set(sub.msgs) | set(sub.enums), [sub_svc], fname)
        hdr = ("// Self-contained subset of %s reconstructed from JS bundles by extract_proto.py.\n"
               "// Methods: %s\n" % (svc, ", ".join(m[0] for m in sel)))
        if foreign:
            hdr += "// NOTE: originally foreign-package types rehomed into %s: %s\n" % (pkg, ", ".join(foreign))
        with open(os.path.join(a.out_dir, fname), "w") as fh:
            fh.write(render_file(sub, pkg, set(sub.msgs) | set(sub.enums), [sub_svc], gopkg(pkg), hdr))
        print("wrote %s (%d types)" % (fname, len(names)), file=sys.stderr)

    if a.json:
        dump = {"messages": {k: [vars(f) for f in v.fields] for k, v in schema.msgs.items()},
                "enums": {k: v.values for k, v in schema.enums.items()},
                "services": {k: v.methods for k, v in schema.services.items()}}
        with open(os.path.join(a.out_dir, "schema.json"), "w") as fh:
            json.dump(dump, fh, indent=1, default=str)
    for w in schema.warnings:
        print("WARNING:", w, file=sys.stderr)


if __name__ == "__main__":
    main()

"""Redis's command specifications, and the argument forms they document.

Redis keeps one JSON file per command and subcommand in src/commands of its
source tree. Each gives the command's arity, flags, key specifications and an
`arguments` tree: a sequence of arguments, each plain (a key, a string, an
integer, a double, a pattern, a unix time), a pure token, a `oneof` of
alternatives, or a `block` of further arguments, and each possibly `optional`,
`multiple` (repeated), or both, with the version that added it in `since`.

This module reads those files as data and turns each tree into the forms a
client can send: the base form (every required argument, each repeated
argument once, each required choice at its first alternative), and then one
form per documented option: each optional argument present, each alternative
of each choice, each repeated argument given twice. Options nested inside an
option are each a form of their own (GEOSEARCH's `COUNT n`, and `COUNT n
ANY`). A form carries the version that introduced what it exercises, so forms
added by a given Redis release can be counted apart.

Every value in a form is concrete, and chosen so that Redis accepts the form
on a small fixture: keys of each type with known contents, numbers in range,
and so on. The fixture is in FIXTURE; the values are chosen by argument name,
then by type, with a short table of exceptions for commands whose arguments
depend on one another (ZRANGE's bounds depend on BYSCORE or BYLEX).

scripts/argument-census.py sends each form to Keel and to Redis and compares
the replies. Nothing here runs anything: the Redis source is read as data.
"""
import json
from pathlib import Path
import re

# The commands whose base form is sent in a session, after these commands on
# the same connection, because they mean nothing on their own.
PRELUDE = {
    'exec': [['MULTI'], ['PING']],
    'discard': [['MULTI']],
}

# Commands never sent to either server, whatever their forms: each would stop,
# block, reconfigure or re-point a server, or change the user that the census
# logs in as.
UNSAFE = {
    'shutdown': 'stops the server',
    'sync': 'starts a full resynchronisation and holds the connection',
    'psync': 'starts a full resynchronisation and holds the connection',
    'replicaof': 'makes the server a replica',
    'slaveof': 'makes the server a replica',
    'failover': 'hands the primary role to a replica',
    'monitor': 'holds the connection as a monitor',
    'debug': 'can crash, sleep or reload the server',
    'client|kill': 'kills connections',
    'client|pause': 'pauses every client',
    'acl|setuser': 'can change the user the census logs in as',
    'acl|deluser': 'can delete the user the census logs in as',
    'acl|load': 'replaces the users',
    'acl|save': 'writes the users file',
    'module|load': 'loads a module',
    'module|loadex': 'loads a module',
    'module|unload': 'unloads a module',
    'cluster|reset': 'resets cluster state',
    'cluster|failover': 'cluster failover',
    'cluster|flushslots': 'drops slot ownership',
    'backup|start': 'writes a backup',
    'backup|seal': 'writes a backup',
    'backup|abort': 'stops a backup',
    'backup|cleanup': 'removes backups',
    'migrate': 'moves keys to another server',
    'sflush': 'flushes slot ranges',
    'swapdb': 'swaps databases under other connections',
}

# Commands whose documented base form is an error on Redis too, so an error
# from both is the expected reply rather than a flaw in the form.
EXPECT_ERROR = {
    'config|rewrite': 'both servers run without a config file',
    # The base form names a command without its arguments, which no command
    # with keys accepts (Redis's getKeysFromCommandWithSpecs arity check).
    'command|getkeys': 'its base form names a command without its arguments',
    'command|getkeysandflags': 'its base form names a command without its arguments',
    # Both servers keep their latency monitors off, so neither has an event
    # to graph and both answer "No samples available".
    'latency|graph': 'latency-monitor-threshold is 0 on both servers, so no event has samples',
}

# The keys each form may name, by type, and the commands that create them. A
# form's first key of a type is the first name, its second the second. A
# destination key (one the command only writes) is a name nothing created.
FIXTURE = {
    'string': (['str', 'str2', 'str3'], [['SET', 'str', 'abc'], ['SET', 'str2', 'abd'], ['SET', 'str3', 'xyz']]),
    'number': (['num', 'num2', 'num3'], [['SET', 'num', '10'], ['SET', 'num2', '20'], ['SET', 'num3', '30']]),
    'list': (['list', 'list2', 'list3'], [['RPUSH', 'list', 'a', 'b', 'c'], ['RPUSH', 'list2', 'b', 'c', 'd'],
                                          ['RPUSH', 'list3', 'c', 'd', 'e']]),
    'hash': (['hash', 'hash2', 'hash3'], [['HSET', 'hash', 'f', 'v', 'n', '5'], ['HSET', 'hash2', 'f', 'w', 'n', '6'],
                                          ['HSET', 'hash3', 'f', 'x', 'n', '7']]),
    'set': (['set', 'set2', 'set3'], [['SADD', 'set', 'a', 'b', 'c'], ['SADD', 'set2', 'b', 'c', 'd'],
                                      ['SADD', 'set3', 'c', 'd', 'e']]),
    'zset': (['zset', 'zset2', 'zset3'], [['ZADD', 'zset', '1', 'a', '2', 'b', '3', 'c'],
                                          ['ZADD', 'zset2', '2', 'b', '3', 'c', '4', 'd'],
                                          ['ZADD', 'zset3', '3', 'c', '4', 'd', '5', 'e']]),
    'geo': (['geo', 'geo2', 'geo3'], [['GEOADD', 'geo', '13.361389', '38.115556', 'Palermo',
                                       '15.087269', '37.502669', 'Catania'],
                                      ['GEOADD', 'geo2', '13.361389', '38.115556', 'Palermo'],
                                      ['GEOADD', 'geo3', '15.087269', '37.502669', 'Catania']]),
    'hll': (['hll', 'hll2', 'hll3'], [['PFADD', 'hll', 'a', 'b', 'c'], ['PFADD', 'hll2', 'b', 'c', 'd'],
                                      ['PFADD', 'hll3', 'c', 'd', 'e']]),
    'stream': (['stream', 'stream2', 'stream3'], [['XADD', 'stream', '1-1', 'f', 'v'], ['XADD', 'stream2', '1-1', 'f', 'v'],
                                                  ['XADD', 'stream3', '1-1', 'f', 'v']]),
    'dest': (['dest', 'dest2', 'dest3'], []),
}

# Which fixture type a command's keys are, by group, then by command.
GROUP_KEY_TYPE = {'string': 'string', 'bitmap': 'string', 'generic': 'string', 'list': 'list', 'hash': 'hash',
                  'set': 'set', 'sorted_set': 'zset', 'geo': 'geo', 'hyperloglog': 'hll', 'stream': 'stream'}
COMMAND_KEY_TYPE = {'incr': 'number', 'decr': 'number', 'incrby': 'number', 'decrby': 'number',
                    'incrbyfloat': 'number', 'increx': 'number', 'sort': 'list', 'sort_ro': 'list'}

# Values by argument name, for the first and the second repetition.
NAME_VALUES = {
    'seconds': ['100', '200'], 'milliseconds': ['100000', '200000'],
    'unix-time-seconds': ['4102444800', '4102444900'], 'unix-time-milliseconds': ['4102444800000', '4102444900000'],
    'count': ['1', '2'], 'num-matches': ['1', '2'], 'offset': ['0', '1'], 'index': ['0', '1'], 'index1': ['0'],
    'index2': ['1'], 'start': ['0', '1'], 'stop': ['-1', '-1'], 'end': ['-1', '-1'], 'cursor': ['0'],
    'increment': ['1', '2'], 'decrement': ['1', '2'], 'timeout': ['1', '1'], 'numreplicas': ['0'], 'numlocal': ['0'],
    'db': ['1'], 'destination-db': ['1'], 'weight': ['1', '2'], 'limit': ['1', '2'], 'rank': ['1'], 'len': ['0'],
    'min-match-len': ['1'], 'bit': ['1'], 'value': ['v', 'w'], 'version': ['6'], 'protover': ['3'], 'port': ['6379'],
    'ttl': ['0'], 'frequency': ['1'], 'size': ['1'], 'slot': ['0', '1'], 'min': ['-inf', '-inf'],
    'max': ['+inf', '+inf'], 'score': ['1.5', '2.5'], 'longitude': ['13.361389', '15.087269'],
    'latitude': ['38.115556', '37.502669'], 'radius': ['200000'], 'width': ['400000'], 'height': ['400000'],
    'float': ['1.5'], 'integer': ['1'], 'lowerbound': ['0'], 'upperbound': ['100'],
    'element': ['a', 'b'], 'member': ['a', 'b'], 'member1': ['a'], 'member2': ['b'], 'field': ['f', 'n'],
    'pattern': ['*', 'a*'], 'by-pattern': ['nosort'], 'get-pattern': ['#', '#'], 'message': ['hi', 'ho'],
    'section': ['server', 'clients'], 'parameter': ['maxmemory', 'maxclients'], 'username': ['default'],
    'libname': ['lib'], 'libver': ['1.0'], 'connection-name': ['c1'], 'clientname': ['c1'], 'type': ['string'],
    'channel': ['ch', 'ch2'], 'shardchannel': ['ch', 'ch2'], 'script': ['return 1'], 'numkeys': ['0'],
    'arg': ['x', 'y'], 'ID': ['1-1', '1-2'], 'id': ['1-1', '1-2'], 'group': ['g'], 'consumer': ['c'],
    'threshold': ['1000'], 'min-idle-time': ['0'], 'encoding': ['u8', 'u8'], 'host': ['127.0.0.1'],
    'event': ['command'], 'command-name': ['get', 'set'], 'command': ['get'], 'category': ['string'],
    'prefix': ['p', 'q'], 'subcommand': ['getreg'], 'fieldset-name': ['fs'], 'pid': ['p1'], 'iid': ['i1'],
    'client-id': ['1', '2'], 'client-type': ['normal'], 'library-name': ['lib'], 'library-name-pattern': ['*'],
    'function': ['f'], 'function-code': ['#!lua name=lib\nredis.register_function("f", function() return 1 end)'],
    'sha1': ['0' * 40], 'last-id': ['1-1'], 'max-deleted-id': ['0-0'], 'entries-added': ['1'],
    'node-id': ['0' * 40], 'key': ['str'], 'bits': ['64'], 'rule': ['on'], 'maxage': ['0'],
    'addr': ['127.0.0.1:1'], 'laddr': ['127.0.0.1:1'], 'old-format': ['127.0.0.1:1'],
    'serialized-value': [''], 'password': ['{password}', '{password}'],
}
TYPE_VALUES = {'integer': ['1', '2'], 'double': ['1.5', '2.5'], 'string': ['s', 't'], 'pattern': ['*', 'a*'],
               'unix-time': ['4102444800', '4102444900']}

# Values for an argument of one command, where the general choice is wrong.
COMMAND_VALUES = {
    ('geodist', 'member1'): ['Palermo'], ('geodist', 'member2'): ['Catania'],
    ('geohash', 'member'): ['Palermo', 'Catania'], ('geopos', 'member'): ['Palermo', 'Catania'],
    ('geosearch', 'member'): ['Palermo'], ('geosearchstore', 'member'): ['Palermo'],
    ('georadiusbymember', 'member'): ['Palermo'], ('georadiusbymember_ro', 'member'): ['Palermo'],
    ('geoadd', 'member'): ['Palermo', 'Catania'],
    ('set', 'ifdeq-digest'): ['0123456789abcdef'], ('set', 'ifdne-digest'): ['0123456789abcdef'],
    ('delex', 'ifdeq-digest'): ['0123456789abcdef'], ('delex', 'ifdne-digest'): ['0123456789abcdef'],
    ('config|set', 'parameter'): ['maxmemory-samples', 'lfu-log-factor'], ('config|set', 'value'): ['5', '10'],
    # A command whose two arguments are a whole call, so the repeated form is one
    # Redis answers (GET with two would be an arity error).
    ('command|getkeys', 'command'): ['mset'], ('command|getkeysandflags', 'command'): ['mset'],
    ('zrange', 'start'): ['0'], ('zrange', 'stop'): ['-1'],
    ('zrangestore', 'min'): ['0'], ('zrangestore', 'max'): ['-1'],
    ('xrange', 'start'): ['-'], ('xrange', 'end'): ['+'], ('xrevrange', 'start'): ['-'], ('xrevrange', 'end'): ['+'],
    ('xpending', 'start'): ['-'], ('xpending', 'end'): ['+'], ('xautoclaim', 'start'): ['0-0'],
    ('xadd', 'id'): ['*'], ('xread', 'ID'): ['0-0', '0-0'], ('xreadgroup', 'ID'): ['>', '>'],
    ('xgroup|create', 'id'): ['$'], ('xgroup|setid', 'id'): ['$'],
    ('zlexcount', 'min'): ['-'], ('zlexcount', 'max'): ['+'], ('zrangebylex', 'min'): ['-'], ('zrangebylex', 'max'): ['+'],
    ('zrevrangebylex', 'min'): ['-'], ('zrevrangebylex', 'max'): ['+'],
    ('zremrangebylex', 'min'): ['-'], ('zremrangebylex', 'max'): ['+'],
    ('lpos', 'element'): ['a'], ('lrem', 'element'): ['a'], ('linsert', 'pivot'): ['a'], ('linsert', 'element'): ['x'],
    ('lset', 'element'): ['x'], ('lrem', 'count'): ['0'],
    ('hincrby', 'field'): ['n'], ('hincrbyfloat', 'field'): ['n'], ('hincrby', 'increment'): ['1'],
    ('setbit', 'offset'): ['0'], ('setbit', 'value'): ['1'], ('getbit', 'offset'): ['0'],
    ('setrange', 'offset'): ['0'], ('bitpos', 'bit'): ['1'],
    ('sort', 'by-pattern'): ['nosort'], ('sort_ro', 'by-pattern'): ['nosort'],
    ('move', 'db'): ['1'], ('select', 'index'): ['0'], ('swapdb', 'index1'): ['0'], ('swapdb', 'index2'): ['1'],
    ('lolwut', 'version'): ['6'], ('wait', 'timeout'): ['1'], ('waitaof', 'timeout'): ['1'],
    ('client|setinfo', 'libname'): ['lib'], ('client|setinfo', 'libver'): ['1.0'],
    ('hello', 'protover'): ['3'], ('info', 'section'): ['server', 'clients'],
    ('scan', 'type'): ['string'], ('scan', 'count'): ['10', '10'],
    ('client|list', 'client-id'): ['1', '2'],
    ('object|freq', 'key'): ['str'], ('expire', 'seconds'): ['100'],
    ('restore', 'ttl'): ['0'], ('copy', 'destination-db'): ['1'],
    ('bitcount', 'start'): ['0'], ('bitcount', 'end'): ['-1'], ('bitpos', 'start'): ['0'], ('bitpos', 'end'): ['-1'],
    ('getrange', 'start'): ['0'], ('getrange', 'end'): ['-1'], ('substr', 'start'): ['0'], ('substr', 'end'): ['-1'],
    ('zrevrangebyscore', 'max'): ['+inf'], ('zrevrangebyscore', 'min'): ['-inf'],
    ('zremrangebyrank', 'start'): ['0'], ('zremrangebyrank', 'stop'): ['0'],
}


def fixup_zrange(argv):
    """ZRANGE's bounds are ranks, scores or lexical bounds by its options, and
    reversed by REV when they are scores or bounds. LIMIT needs BYSCORE or
    BYLEX, so the LIMIT form is sent by score."""
    upper = [a.upper() for a in argv]
    if 'LIMIT' in upper[4:] and 'BYSCORE' not in upper[4:] and 'BYLEX' not in upper[4:]:
        argv.insert(4, 'BYSCORE')
        upper.insert(4, 'BYSCORE')
    rev = 'REV' in upper[4:]
    if 'BYSCORE' in upper[4:]:
        argv[2:4] = ['+inf', '-inf'] if rev else ['-inf', '+inf']
    elif 'BYLEX' in upper[4:]:
        argv[2:4] = ['+', '-'] if rev else ['-', '+']
    return argv


def fixup_lcs(argv):
    """LCS LEN with IDX is an error ('please just use IDX'); the one form that
    documents both is sent with IDX alone, which is what Redis asks for."""
    upper = [a.upper() for a in argv]
    if 'LEN' in upper and 'IDX' in upper:
        argv = [a for a in argv if a.upper() != 'LEN']
    return argv


def fixup_zadd(argv):
    """ZADD INCR takes a single score and member; NX with GT or LT is an error."""
    upper = [a.upper() for a in argv]
    options = [a for a in upper[2:] if a in ('NX', 'XX', 'GT', 'LT', 'CH', 'INCR')]
    pairs = argv[2 + len(options):]
    if 'INCR' in options and len(pairs) > 2:
        argv = argv[:2 + len(options)] + pairs[:2]
    return argv


def fixup_weights(argv):
    """ZUNION's WEIGHTS must count as many as its keys: the two-weight form
    names two keys."""
    upper = [a.upper() for a in argv]
    if 'WEIGHTS' not in upper:
        return argv
    at = upper.index('WEIGHTS')
    weights = 0
    for a in upper[at + 1:]:
        if a in ('AGGREGATE', 'WITHSCORES'):
            break
        weights += 1
    first = 2 if upper[0].endswith('STORE') else 1  # ZUNIONSTORE dest numkeys ...
    numkeys = int(argv[first])
    if weights > numkeys:
        keys = FIXTURE['zset'][0][:weights]
        argv = argv[:first] + [str(weights)] + keys + argv[first + 1 + numkeys:]
    return argv


FIXUPS = {'zrange': fixup_zrange, 'zrangestore': fixup_zrange, 'lcs': fixup_lcs, 'zadd': fixup_zadd,
          'zunion': fixup_weights, 'zinter': fixup_weights, 'zunionstore': fixup_weights,
          'zinterstore': fixup_weights}


def version(text):
    """A version string as a tuple, for comparison; '' is the earliest."""
    return tuple(int(p) for p in text.split('.')) if text else ()


def load_specs(directory):
    """Every command and subcommand in a src/commands directory, by its full
    lower-case name (a subcommand as container|sub)."""
    specs = {}
    for path in sorted(Path(directory).glob('*.json')):
        with path.open() as f:
            data = json.load(f)
        for name, spec in data.items():
            full = f"{spec['container']}|{name}".lower() if 'container' in spec else name.lower()
            spec = dict(spec)
            spec['name'] = full
            spec['file'] = path.name
            specs[full] = spec
    containers = {s['container'].lower() for s in specs.values() if 'container' in s}
    for name, spec in specs.items():
        spec['is_container'] = name in containers
    return specs


class Key:
    """A key argument, to be named when the form is materialized."""

    def __init__(self, flags, rep):
        self.flags, self.rep = flags, rep


class Val:
    """A plain value, chosen by argument name and type when materialized."""

    def __init__(self, name, type_, rep):
        self.name, self.type, self.rep = name, type_, rep


class Count:
    """A count of a repeated argument that follows it (numkeys, numfields)."""

    def __init__(self, name):
        self.name = name


# Which repeated argument each count counts; one not listed counts the next
# repeated argument after it.
COUNTS = {'numkeys': 'key', 'numfields': 'field', 'numids': 'id', 'numranges': None}


class Form:
    """One way of sending a command's argument node: its label (the option it
    exercises, or '' for the base form), the version that introduced that,
    its tokens, and for a repeated node how many repetitions it holds."""

    def __init__(self, label, since, tokens, reps=None):
        self.label, self.since, self.tokens, self.reps = label, since, list(tokens), reps

    def __repr__(self):
        return f'Form({self.label!r}, {self.since!r}, {self.tokens!r}, {self.reps!r})'


def later(a, b):
    return a if version(a) >= version(b) else b


def node_label(node):
    """What to call a node in a form's label: its token, or for a choice its
    first alternative's label, for a block its first required child's."""
    if node.get('token'):
        return node['token']
    if node.get('type') == 'oneof':
        return node_label(node['arguments'][0])
    if node.get('type') == 'block':
        required = [a for a in node['arguments'] if not a.get('optional')] or node['arguments']
        return node_label(required[0])
    return node['name']


def render(node, rep=0):
    """Every form of one argument node: the base first, then one per option.

    An optional node's base is empty. A repeated node's forms are its single
    repetition's, then one with two repetitions. A token (EX, LIMIT, WEIGHTS)
    goes before the node's value, or before each repetition when the
    documentation says so (SORT's GET)."""
    token = node.get('token') if node.get('type') != 'pure-token' else None
    since = node.get('since', '')
    core = render_core(node, rep)
    if node.get('multiple'):
        second = render_core(node, 1)[0]
        if token and node.get('multiple_token'):
            core = [Form(f.label, f.since, [token] + f.tokens, 1) for f in core]
            twice = Form(f"{node['name']}*2", since, core[0].tokens + [token] + second.tokens, 2)
        elif token:
            twice = Form(f"{node['name']}*2", since, [token] + core[0].tokens + second.tokens, 2)
            core = [Form(f.label, f.since, [token] + f.tokens, 1) for f in core]
        else:
            twice = Form(f"{node['name']}*2", since, core[0].tokens + second.tokens, 2)
            core = [Form(f.label, f.since, f.tokens, 1) for f in core]
        core = core + [twice]
    elif token:
        core = [Form(f.label, f.since, [token] + f.tokens) for f in core]
    if node.get('optional'):
        label = node_label(node)
        # A choice's alternatives are already named by their own labels.
        prefix = '' if node.get('type') == 'oneof' else label
        return [Form('', '', [], 0 if node.get('multiple') else None)] + [
            Form(label if not f.label else f'{prefix} {f.label}'.strip(), later(since, f.since), f.tokens, f.reps)
            for f in core]
    return core


def render_core(node, rep):
    """The forms of a node as if it were required and given once, without its
    own token."""
    kind = node.get('type')
    since = node.get('since', '')
    if kind == 'pure-token':
        return [Form('', since, [node.get('token') or node['name']])]
    if kind == 'oneof':
        forms = []
        for i, alt in enumerate(node['arguments']):
            for j, f in enumerate(render(alt, rep)):
                if i == 0 and j == 0:
                    forms.insert(0, Form('', later(since, f.since), f.tokens))
                    continue
                label = node_label(alt)
                forms.append(Form(label if not f.label else f'{label} {f.label}', later(since, f.since), f.tokens))
        return forms
    if kind == 'block':
        return [Form(f.label, later(since, f.since), f.tokens) for f in sequence(node['arguments'], rep)]
    if kind == 'key':
        return [Form('', since, [Key(node.get('key_spec_flags', ()), rep)])]
    if kind == 'integer' and node['name'] in COUNTS:
        return [Form('', since, [Count(node['name'])])]
    return [Form('', since, [Val(node['name'], kind, rep)])]


def sequence(children, rep=0):
    """The forms of a sequence of nodes: every child at its base, then each
    child's options one at a time with the others at their base."""
    rendered = [render(child, rep) for child in children]
    names = [child['name'] for child in children]
    bases = [forms[0] for forms in rendered]
    out = [Form('', '', resolve_counts([(b.tokens, b.reps, n) for b, n in zip(bases, names)]))]
    for i, forms in enumerate(rendered):
        for f in forms[1:]:
            parts = [(b.tokens, b.reps, n) for b, n in zip(bases, names)]
            parts[i] = (f.tokens, f.reps, names[i])
            out.append(Form(f.label, f.since, resolve_counts(parts)))
    return out


def resolve_counts(parts):
    """Each Count becomes the number of repetitions of the repeated argument it
    counts, among the parts after it (0 when that argument is absent), and the
    parts join into one token list."""
    tokens = []
    for i, (part, _, _) in enumerate(parts):
        for t in part:
            if isinstance(t, Count):
                target = COUNTS[t.name]
                following = [(reps, name) for _, reps, name in parts[i + 1:] if reps is not None]
                named = [reps for reps, name in following if name == target]
                t = str(named[0] if named else following[0][0] if following else 0)
            tokens.append(t)
    return tokens


def annotate_keys(spec):
    """Give every key argument its key spec's flags, so that a destination key
    (written, never read) can be told from one the fixture must hold."""
    key_specs = spec.get('key_specs', [])

    def walk(args):
        for a in args:
            if a.get('type') == 'key':
                index = a.get('key_spec_index', 0)
                a['key_spec_flags'] = tuple(key_specs[index].get('flags', ())) if index < len(key_specs) else ()
            if 'arguments' in a:
                walk(a['arguments'])
    walk(spec.get('arguments', []))


def forms_of(spec, password='{password}'):
    """Every form of a command, materialized: argv as strings. The base form
    is first. Each form has label, since (the version that introduced the
    option, or the command) and argv."""
    annotate_keys(spec)
    name = spec['name']
    out = []
    for f in sequence(spec.get('arguments', [])):
        argv = materialize(name, spec, f.tokens, password)
        if name in FIXUPS:
            argv = FIXUPS[name](argv)
        out.append({'label': f.label, 'since': later(spec.get('since', ''), f.since), 'argv': argv})
    return out


def key_type(spec, key):
    if 'OW' in key.flags and 'ACCESS' not in key.flags:
        return 'dest'
    name = spec['name']
    return COMMAND_KEY_TYPE.get(name) or GROUP_KEY_TYPE.get(spec.get('group'), 'string')


def materialize(name, spec, tokens, password='{password}'):
    """Concrete argv: the command's words, then each token as a string. The
    first key of a type is the fixture's first key of that type, the second
    its second."""
    argv = [w.upper() for w in name.split('|')]
    used = {}
    for t in tokens:
        if isinstance(t, Key):
            type_ = key_type(spec, t)
            names = FIXTURE[type_][0]
            n = used.get(type_, 0)
            used[type_] = n + 1
            argv.append(names[min(n, len(names) - 1)])
        elif isinstance(t, Val):
            choices = (COMMAND_VALUES.get((name, t.name)) or NAME_VALUES.get(t.name) or
                       TYPE_VALUES.get(t.type) or ['s', 't'])
            argv.append(choices[min(t.rep, len(choices) - 1)].replace('{password}', password))
        else:
            argv.append(str(t))
    return argv


def fixture_commands(present):
    """The commands that build the fixture, limited to those a server has
    (a server without XADD simply has no stream keys, on both sides)."""
    out = []
    for names, commands in FIXTURE.values():
        for command in commands:
            if command[0].lower() in present:
                out.append(command)
    return out


UNKNOWN = re.compile(r'^ERR unknown (command|subcommand)\b', re.I)


def encode(parts):
    """A command as a RESP array of bulk strings."""
    parts = [p if isinstance(p, bytes) else str(p).encode() for p in parts]
    return b'*%d\r\n' % len(parts) + b''.join(b'$%d\r\n' % len(p) + p + b'\r\n' for p in parts)


def read_reply(stream, depth=0):
    """One RESP2 or RESP3 reply as a tree: (kind, value). Kinds are the
    protocol's type bytes: '+' and '-' carry text, ':' and ',' and '(' their
    number as text, '_' None, '#' a bool, '$' '=' '!' text or None for a null
    bulk, '*' '~' '>' a list or None, '%' a list of (key, value) pairs, and
    '|' (an attribute) a pair list with the reply it decorates."""
    if depth > 32:
        raise ValueError('RESP nesting limit')
    line = stream.readline(1 << 20)
    if not line.endswith(b'\r\n'):
        raise ConnectionError('connection closed mid-reply' if not line else f'invalid RESP header {line[:40]!r}')
    kind, body = line[:1].decode(), line[1:-2]
    if kind in '+-:,(':
        return kind, body.decode('utf-8', 'replace')
    if kind == '_':
        return kind, None
    if kind == '#':
        return kind, body == b't'
    if kind in '$=!':
        n = int(body)
        if n < 0:
            return kind, None
        payload = stream.read(n + 2)
        if len(payload) != n + 2:
            raise ConnectionError('short bulk reply')
        return kind, payload[:-2].decode('utf-8', 'replace')
    if kind in '*~>':
        n = int(body)
        return kind, None if n < 0 else [read_reply(stream, depth + 1) for _ in range(n)]
    if kind in '%|':
        pairs = [(read_reply(stream, depth + 1), read_reply(stream, depth + 1)) for _ in range(int(body))]
        if kind == '|':
            return kind, (pairs, read_reply(stream, depth + 1))
        return kind, pairs
    raise ValueError(f'unknown RESP type {line[:40]!r}')


def text(reply, limit=160):
    """A reply as one short line, in a notation close to the protocol's:
    +OK, :1, $abc, _, *[:1 $a], %{$k: :1}, -ERR ..."""
    kind, value = reply
    if kind in '*~>':
        body = '[' + ' '.join(text(v, limit) for v in value) + ']' if value is not None else '-1'
    elif kind == '%':
        body = '{' + ' '.join(f'{text(k, limit)}: {text(v, limit)}' for k, v in value) + '}'
    elif kind == '|':
        body = '{' + ' '.join(f'{text(k, limit)}: {text(v, limit)}' for k, v in value[0]) + '} ' + text(value[1], limit)
    elif value is None:
        body = '-1' if kind in '$=!' else ''
    elif kind == '#':
        body = 't' if value else 'f'
    else:
        body = str(value).replace('\r', '\\r').replace('\n', '\\n')
    out = kind + body
    return out if len(out) <= limit else out[:limit - 3] + '...'

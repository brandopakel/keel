import importlib.util
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('config_census', Path(__file__).with_name('config-census.py'))
cc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cc)

CONFIG_C = '''
standardConfig static_configs[] = {
    /* Bool configs */
    createBoolConfig("protected-mode", NULL, MODIFIABLE_CONFIG, server.protected_mode, 1, NULL, NULL),
    createBoolConfig("lazyfree-lazy-eviction", NULL, DEBUG_CONFIG | MODIFIABLE_CONFIG, server.lazyfree_lazy_eviction, 0, NULL, NULL),
#ifdef USE_COMPRESSION
    createIntConfig("repl-compression", NULL, IMMUTABLE_CONFIG, 0, 22, server.repl_compression, 0, INTEGER_CONFIG, NULL, NULL),
#endif
    createSpecialConfig("replicaof", "slaveof", IMMUTABLE_CONFIG | MULTI_ARG_CONFIG, setConfigReplicaOfOption, getConfigReplicaOfOption, rewriteConfigReplicaOfOption, NULL),
    createSizeTConfig("maxmemory", NULL, MODIFIABLE_CONFIG, 0, ULONG_MAX, server.maxmemory, 0, MEMORY_CONFIG, NULL, updateMaxmemory),
    /* NULL Terminator, this is dropped when generating the module config, so no actual conf */
    {NULL}
};

int registerConfigValue(const char *name) {
    createBoolConfig("not-in-the-table", NULL, 0, x, 0, NULL, NULL);
}
'''

REDIS_CONF = '''# Redis configuration file example.
#
# protected-mode yes
# maxmemory <bytes>
# The maxmemory directive above is an example in a comment.
#maxmemory-policy noeviction
replicaof 127.0.0.1 6379
# Not a directive: lazyfree-lazy-eviction is mentioned in prose here.
'''

TESTS = {
    'unit/maxmemory.tcl': 'r config set maxmemory 100mb\nr config set maxmemory-policy allkeys-lru\n',
    'unit/other.tcl': 'start_server {tags {"other"} overrides {maxmemory 0 save ""}} {\n}\n',
    'integration/replication.tcl': 'start_server {overrides {replicaof {127.0.0.1 1}}} {\nconfig_set maxmemory 1\n}',
}


class Table(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        (self.root / 'src').mkdir()
        (self.root / 'src/config.c').write_text(CONFIG_C)
        (self.root / 'redis.conf').write_text(REDIS_CONF)
        for name, text in TESTS.items():
            path = self.root / 'tests' / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(text)

    def tearDown(self):
        self.tmp.cleanup()

    def test_directives_with_alias_flags_and_build_condition(self):
        rows = cc.parse_table(self.root / 'src/config.c')
        self.assertEqual([r['name'] for r in rows],
                         ['protected-mode', 'lazyfree-lazy-eviction', 'repl-compression', 'replicaof', 'maxmemory'],
                         'the table ends at its terminator')
        by = {r['name']: r for r in rows}
        self.assertEqual((by['replicaof']['alias'], by['replicaof']['kind'], by['replicaof']['flags']),
                         ('slaveof', 'special', ['IMMUTABLE_CONFIG', 'MULTI_ARG_CONFIG']))
        self.assertEqual(by['lazyfree-lazy-eviction']['flags'], ['DEBUG_CONFIG'])
        self.assertEqual(by['repl-compression']['condition'], '#ifdef USE_COMPRESSION')
        self.assertEqual(by['maxmemory']['condition'], '')

    def test_redis_conf_documents(self):
        names = {'protected-mode', 'maxmemory', 'maxmemory-policy', 'replicaof', 'lazyfree-lazy-eviction'}
        documented = cc.parse_conf(self.root / 'redis.conf', names)
        self.assertEqual(documented, {'protected-mode': ('example', '# protected-mode yes'),
                                      'maxmemory': ('example', '# maxmemory <bytes>'),
                                      'maxmemory-policy': ('example', '#maxmemory-policy noeviction'),
                                      'replicaof': ('active', 'replicaof 127.0.0.1 6379')})

    def test_test_suite_uses(self):
        uses = cc.test_uses(self.root / 'tests', {'maxmemory', 'maxmemory-policy', 'save', 'replicaof'})
        self.assertEqual(uses, {'maxmemory': 3, 'maxmemory-policy': 1, 'save': 1, 'replicaof': 1})


class Classes(unittest.TestCase):
    def test_has_mapped_lacks(self):
        self.assertEqual(cc.classify({'name': 'maxmemory'}, '0', set()), ('has', ''))
        self.assertEqual(cc.classify({'name': 'lfu-decay-time'}, None, {'lfu-decay-period'})[0], 'mapped')
        self.assertEqual(cc.classify({'name': 'tls-port'}, None, set()), ('lacks', ''))

    def test_families(self):
        for name, label in [('cluster-enabled', 'cluster'), ('tls-port', 'tls'), ('repl-backlog-size', 'replication'),
                            ('replicaof', 'replication'), ('appendonly', 'persistence'), ('save', 'persistence'),
                            ('maxmemory-policy', 'memory and eviction'), ('hash-max-listpack-entries', 'encodings'),
                            ('tcp-backlog', 'clients and network'), ('loglevel', 'logging and process'),
                            ('requirepass', 'security'), ('lua-time-limit', 'scripting'),
                            ('slowlog-log-slower-than', 'monitoring'), ('databases', 'other')]:
            self.assertEqual(cc.family(name), label, name)

    def test_summary_leaves_out_what_this_build_lacks(self):
        rows = [
            {'name': 'maxmemory', 'class': 'has', 'documented': 'example', 'keel_value': '0', 'redis_value': '0',
             'used_by': ['x'], 'test_files': 3, 'family': 'memory and eviction', 'condition': ''},
            {'name': 'tls-port', 'class': 'lacks', 'documented': 'example', 'keel_value': None, 'redis_value': '0',
             'used_by': [], 'test_files': 5, 'family': 'tls', 'condition': ''},
            {'name': 'loglevel', 'class': 'lacks', 'documented': 'active', 'keel_value': None, 'redis_value': 'notice',
             'used_by': ['docker'], 'test_files': 0, 'family': 'logging and process', 'condition': ''},
            {'name': 'repl-compression', 'class': 'lacks', 'documented': 'no', 'keel_value': None, 'redis_value': None,
             'used_by': [], 'test_files': 0, 'family': 'replication', 'condition': '#ifdef USE_COMPRESSION'},
        ]
        summary = cc.summarise(rows)
        self.assertEqual(summary['total'], {'configs': 3, 'has': 1, 'mapped': 0, 'lacks': 2, 'lacks_used': 1,
                                            'lacks_tested': 1, 'documented': 3, 'same_value': 1})
        self.assertEqual(summary['families']['tls'], {'configs': 1, 'has': 0, 'mapped': 0, 'lacks': 1})
        self.assertNotIn('replication', summary['families'])
        text = cc.markdown(summary, rows, '8.10.1')
        self.assertIn('**3 configuration directives**', text)
        self.assertIn('| `loglevel` | docker | `notice` | 0 |', text)
        self.assertIn('| `tls-port` | 5 | `0` |', text)
        self.assertNotIn('repl-compression', text)


if __name__ == '__main__':
    unittest.main()

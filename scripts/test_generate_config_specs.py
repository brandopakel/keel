import importlib.util
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('generate_config_specs', Path(__file__).with_name('generate-config-specs.py'))
gcs = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gcs)

# A trimmed src/config.c and server.h, with every kind of entry the generator
# reads and the C it has to evaluate: macros, an enum, casts and suffixes.
SERVER_H = '''
#define CONFIG_DEFAULT_HZ 10 /* Time interrupt calls/sec. */
#define MAXMEMORY_FLAG_LRU (1<<0)
#define MAXMEMORY_ALLKEYS_LRU ((0<<8)|MAXMEMORY_FLAG_LRU)
#define MAXMEMORY_NO_EVICTION (7<<8)
#define TWICE 1
#define TWICE 2
#define CONFIG_DEFAULT_TITLE "{title} {port}"
typedef enum {
    SANITIZE_DUMP_NO = 0,
    SANITIZE_DUMP_YES,
    SANITIZE_DUMP_CLIENTS
} sanitize;
'''

CONFIG_C = '''
configEnum maxmemory_policy_enum[] = {
    {"allkeys-lru",MAXMEMORY_ALLKEYS_LRU},
    {"noeviction",MAXMEMORY_NO_EVICTION},
    {NULL, 0}
};
configEnum sanitize_dump_payload_enum[] = {
    {"no", SANITIZE_DUMP_NO},
    {"yes", SANITIZE_DUMP_YES},
    {"clients", SANITIZE_DUMP_CLIENTS},
    {NULL, 0}
};
configEnum flags_enum[] = {
    {"no", 0},
    {"yes", 3},
    {"a", 1},
    {"b", 2},
    {NULL, 0}
};

standardConfig static_configs[] = {
    createBoolConfig("daemonize", NULL, IMMUTABLE_CONFIG, server.daemonize, 0, NULL, NULL),
    createStringConfig("proc-title-template", NULL, MODIFIABLE_CONFIG, ALLOW_EMPTY_STRING, server.t, CONFIG_DEFAULT_TITLE, isValidProcTitleTemplate, updateProcTitleTemplate),
    createSDSConfig("requirepass", NULL, MODIFIABLE_CONFIG | SENSITIVE_CONFIG, EMPTY_STRING_IS_NULL, server.requirepass, NULL, NULL, updateRequirePass),
    createEnumConfig("maxmemory-policy", NULL, MODIFIABLE_CONFIG, maxmemory_policy_enum, server.maxmemory_policy, MAXMEMORY_NO_EVICTION, NULL, NULL),
    createEnumConfig("sanitize-dump-payload", NULL, DEBUG_CONFIG | MODIFIABLE_CONFIG, sanitize_dump_payload_enum, server.s, SANITIZE_DUMP_NO, NULL, NULL),
    createEnumConfig("bits", NULL, MODIFIABLE_CONFIG | MULTI_ARG_CONFIG, flags_enum, server.b, 3, NULL, NULL),
    createIntConfig("hz", NULL, MODIFIABLE_CONFIG, 0, INT_MAX, server.config_hz, CONFIG_DEFAULT_HZ, INTEGER_CONFIG, NULL, updateHZ),
    createIntConfig("list-max-listpack-size", "list-max-ziplist-size", MODIFIABLE_CONFIG, INT_MIN, INT_MAX, server.l, -2, INTEGER_CONFIG, NULL, NULL),
    createUIntConfig("unixsocketperm", NULL, IMMUTABLE_CONFIG, 0, 0777, server.unixsocketperm, 0, OCTAL_CONFIG, NULL, NULL),
#ifdef USE_THING
    createLongLongConfig("proto-max-bulk-len", NULL, DEBUG_CONFIG | MODIFIABLE_CONFIG, 1024*1024, LONG_MAX, server.p, 512ll*1024*1024, MEMORY_CONFIG, NULL, NULL), /* Bulk */
#endif
    createULongLongConfig("maxmemory", NULL, MODIFIABLE_CONFIG, 0, ULLONG_MAX, server.maxmemory, 0, MEMORY_CONFIG, NULL, updateMaxmemory),
    createSSizeTConfig("maxmemory-clients", NULL, MODIFIABLE_CONFIG, -100, SSIZE_MAX, server.mc, 0, MEMORY_CONFIG | PERCENT_CONFIG, NULL, apply),
    createSizeTConfig("active-defrag-ignore-bytes", NULL, MODIFIABLE_CONFIG, 1, LLONG_MAX, server.a, 100<<20, MEMORY_CONFIG, NULL, NULL),
    createSpecialConfig("save", NULL, MODIFIABLE_CONFIG | MULTI_ARG_CONFIG, setConfigSaveOption, getConfigSaveOption, rewriteConfigSaveOption, NULL),
};
'''


def source(tmp, config_c=CONFIG_C, server_h=SERVER_H):
    src = Path(tmp) / 'src'
    src.mkdir()
    (src / 'config.c').write_text(config_c)
    (src / 'server.h').write_text(server_h)
    return tmp


class Collect(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.specs = {s['name']: s for s in gcs.collect(source(self.tmp.name))}

    def tearDown(self):
        self.tmp.cleanup()

    def test_every_entry_in_order_with_its_flags(self):
        self.assertEqual(list(self.specs), ['daemonize', 'proc-title-template', 'requirepass', 'maxmemory-policy',
                                            'sanitize-dump-payload', 'bits', 'hz', 'list-max-listpack-size',
                                            'unixsocketperm', 'proto-max-bulk-len', 'maxmemory', 'maxmemory-clients',
                                            'active-defrag-ignore-bytes', 'save'])
        self.assertEqual(self.specs['daemonize']['flags'], ['configImmutable'])
        self.assertEqual(self.specs['requirepass']['flags'], ['configSensitive'])
        self.assertEqual(self.specs['list-max-listpack-size']['alias'], 'list-max-ziplist-size')
        self.assertEqual(self.specs['proto-max-bulk-len']['condition'], '#ifdef USE_THING')
        self.assertEqual(self.specs['hz']['apply'], 'updateHZ')
        self.assertEqual(self.specs['proc-title-template']['valid'], 'isValidProcTitleTemplate')

    def test_defaults_as_config_get_writes_them(self):
        want = {'daemonize': 'no', 'proc-title-template': '{title} {port}', 'requirepass': '',
                'maxmemory-policy': 'noeviction', 'sanitize-dump-payload': 'no', 'bits': 'yes', 'hz': '10',
                'list-max-listpack-size': '-2', 'unixsocketperm': '0', 'proto-max-bulk-len': '536870912',
                'maxmemory': '0', 'maxmemory-clients': '0', 'active-defrag-ignore-bytes': '104857600'}
        self.assertEqual({n: s['default'] for n, s in self.specs.items() if n != 'save'}, want)
        self.assertTrue(self.specs['requirepass']['empty_is_null'])

    def test_bounds_as_a_long_long_holds_them(self):
        self.assertEqual((self.specs['hz']['lower'], self.specs['hz']['upper']), (0, 2**31 - 1))
        self.assertEqual(self.specs['maxmemory']['upper'], -1, 'ULLONG_MAX in a long long')
        self.assertEqual(self.specs['unixsocketperm']['upper'], 0o777)
        self.assertEqual(self.specs['maxmemory-clients']['numflags'], ['numMemory', 'numPercent'])
        self.assertEqual(self.specs['maxmemory-clients']['lower'], -100)

    def test_enums_read_through_macros_and_c_enums(self):
        self.assertEqual(self.specs['maxmemory-policy']['enum'], [('allkeys-lru', 1), ('noeviction', 7 << 8)])
        self.assertEqual(self.specs['sanitize-dump-payload']['enum'], [('no', 0), ('yes', 1), ('clients', 2)])

    def test_a_macro_defined_twice_differently_is_refused(self):
        with tempfile.TemporaryDirectory() as tmp:
            bad = CONFIG_C.replace('CONFIG_DEFAULT_HZ, INTEGER_CONFIG', 'TWICE, INTEGER_CONFIG')
            with self.assertRaisesRegex(ValueError, 'no single definition of TWICE'):
                gcs.collect(source(tmp, config_c=bad))


class Render(unittest.TestCase):
    def test_writes_gofmt_go(self):
        with tempfile.TemporaryDirectory() as tmp:
            text = gcs.render(gcs.collect(source(tmp)))
        self.assertTrue(text.startswith('// Code generated by scripts/generate-config-specs.py'))
        self.assertIn('{name: "maxmemory", kind: configNumeric, num: numULongLong, numFlags: numMemory, '
                      'lower: 0, upper: -1, def: "0", apply: "updateMaxmemory"},', text)
        self.assertIn('{name: "bits", kind: configEnum, flags: configMultiArg, '
                      'enum: []configEnumValue{{"no", 0}, {"yes", 3}, {"a", 1}, {"b", 2}}, def: "yes"},', text)
        self.assertIn('{name: "save", kind: configSpecial, flags: configMultiArg},', text)


class Helpers(unittest.TestCase):
    def test_numbers_as_config_get_writes_them(self):
        self.assertEqual(gcs.numeric_get(-50, ['numMemory', 'numPercent']), '50%')
        self.assertEqual(gcs.numeric_get(-1, ['numMemory']), '18446744073709551615')
        self.assertEqual(gcs.numeric_get(0o750, ['numOctal']), '750')
        self.assertEqual(gcs.numeric_get(-2, []), '-2')

    def test_enum_names_for_bit_flags(self):
        values = [('no', 0), ('yes', 3), ('a', 1), ('b', 2)]
        self.assertEqual(gcs.enum_name(values, 1, True), 'a')
        self.assertEqual(gcs.enum_name(values, 3, True), 'yes')
        self.assertEqual(gcs.enum_name(values, 4, True), 'unknown')
        self.assertEqual(gcs.enum_name([('x', 1), ('y', 2)], 3, True), 'x y')

    def test_c_casts(self):
        self.assertEqual(gcs.c_cast(2**32 - 1, 32, True), -1)
        self.assertEqual(gcs.c_cast(-1, 32, False), 2**32 - 1)


if __name__ == '__main__':
    unittest.main()

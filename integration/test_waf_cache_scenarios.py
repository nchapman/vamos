import unittest
from unittest.mock import patch

import waf_cache_scenarios as scenario


class CacheEvidenceTests(unittest.TestCase):
    def response(self, identity, cache_status='Souin; hit', body=b'origin response'):
        return 200, {'x-origin-id': identity, 'cache-status': cache_status}, body

    def test_hit_requires_matching_origin_identity_body_and_header(self):
        response = self.response('a')
        with patch.object(scenario, 'request', side_effect=[response, response]):
            self.assertEqual(scenario.cache_pair(None, 'host', '/cache', True), response)
        for second in (self.response('b'), self.response('a', 'Souin; fwd=uri-miss'),
                       self.response('a', body=b'changed')):
            with self.subTest(second=second), patch.object(scenario, 'request', side_effect=[response, second]):
                with self.assertRaises(AssertionError):
                    scenario.cache_pair(None, 'host', '/cache', True)

    def test_disabled_cache_requires_distinct_origin_responses(self):
        with patch.object(scenario, 'request', side_effect=[self.response('a'), self.response('b')]):
            scenario.cache_pair(None, 'host', '/cache', False)
        with patch.object(scenario, 'request', return_value=self.response('a')):
            with self.assertRaises(AssertionError):
                scenario.cache_pair(None, 'host', '/cache', False)


if __name__ == '__main__':
    unittest.main()

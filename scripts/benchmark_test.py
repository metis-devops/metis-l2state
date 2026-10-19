"""Fail-closed acceptance for byte-identical and cross-encoding comparisons."""
import copy
import unittest
from benchmark import check_observations, validate_allocation_sample


class ComparisonTests(unittest.TestCase):
    def setUp(self):
        self.items = [dict(final_root="root", summary={"FinalStateRoot": "root", "Counts": 1},
                           implementation=name, artifact_sha256=digest)
                      for name, digest in [("baseline", "old"), ("current", "new"), ("current", "new")]]

    def test_explicit_encoding_change(self):
        check_observations(self.items, "summary")
        with self.assertRaises(RuntimeError):
            check_observations(self.items, "bytes")

    def test_nondeterministic_encoding(self):
        self.items[-1]["artifact_sha256"] = "different"
        with self.assertRaises(RuntimeError):
            check_observations(self.items, "summary")

    def test_full_summary_and_root_required(self):
        for field in ["summary", "final_root"]:
            items = copy.deepcopy(self.items)
            items[-1][field] = {"FinalStateRoot": "root", "Counts": 2} if field == "summary" else "wrong"
            with self.assertRaises(RuntimeError):
                check_observations(items, "summary")

    def test_cache_modes_share_encoding(self):
        self.items[1]["implementation"] = "current-memory"
        self.items[2]["implementation"] = "current-none"
        for item in self.items[1:]:
            item["encoding"] = "current"
        check_observations(self.items, "summary")
        self.items[2]["artifact_sha256"] = "different-cache-output"
        with self.assertRaises(RuntimeError):
            check_observations(self.items, "summary")

    def test_default_bytes_match(self):
        for item in self.items:
            item["artifact_sha256"] = "same"
        check_observations(self.items, "bytes")


class AllocationTests(unittest.TestCase):
    def test_requires_requested_benchmark(self):
        raw = "BenchmarkDump-16 1 123 ns/op 456 B/op 7 allocs/op\nPASS\n"
        validate_allocation_sample(raw, "BenchmarkDump")
        for bad in ["PASS\n", raw.replace("BenchmarkDump", "BenchmarkMigrate"), raw + raw, raw.replace("PASS", "FAIL")]:
            with self.assertRaises(RuntimeError):
                validate_allocation_sample(bad, "BenchmarkDump")


if __name__ == "__main__":
    unittest.main()

#!/usr/bin/env python3
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
"""Tests of hack/security_report.py: the gate rules and the honesty of results.json."""

import contextlib
import io
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import security_report as sr  # noqa: E402


def env(scanner, findings=None, status="ok", target="", optional=False):
    e = sr.envelope(scanner, status, "because" if status != "ok" else "", target=target)
    e["findings"] = findings or []
    if optional:
        e["optional"] = True
    return e


def vuln(ident, severity, fix, pkg="libfoo"):
    return sr.finding(id=ident, severity=severity, package=pkg, version="1", fix=fix, fixed_in=["2"] if fix == "fixed" else [])


def merged(*envs):
    return {"schema": 1, "generated_at": "now", "envelopes": list(envs)}


class Gate(unittest.TestCase):
    def test_fixable_critical_blocks(self):
        out = sr.violations(merged(env("grype", [vuln("CVE-1", "critical", "fixed")], target="console")), None, "push")
        self.assertEqual(len(out), 1)

    def test_unfixable_critical_does_not_block_but_stays_listed(self):
        m = merged(env("grype", [vuln("CVE-1", "critical", "none")], target="console"))
        self.assertEqual(sr.violations(m, None, "push"), [])
        rows = sr.build_results(m, None)["findings"]["rows"]
        self.assertEqual([r["id"] for r in rows], ["CVE-1"])
        self.assertEqual(rows[0]["fix"]["state"], "none")
        self.assertIn("no fix yet", rows[0]["fix"]["label"])

    def test_new_fixable_high_blocks_only_pull_requests_with_a_baseline(self):
        m = merged(env("grype", [vuln("CVE-2", "high", "fixed")], target="console"))
        self.assertEqual(sr.violations(m, set(), "push"), [])
        self.assertEqual(sr.violations(m, None, "pull_request"), [])
        self.assertEqual(len(sr.violations(m, set(), "pull_request")), 1)
        known = {sr.finding_key(m["envelopes"][0], m["envelopes"][0]["findings"][0])}
        self.assertEqual(sr.violations(m, known, "pull_request"), [])

    def test_accepted_finding_does_not_block_but_stays_visible(self):
        m = merged(env("grype", [vuln("CVE-1", "critical", "fixed")], target="console"))
        accepted = {("grype", "CVE-1", "*"): "the vulnerable code is not compiled in"}
        self.assertEqual(sr.violations(m, None, "push", accepted), [])
        out = sr.build_results(m, None, accepted)
        self.assertEqual(out["findings"]["rows"][0]["fix"]["state"], "accepted")
        self.assertIn("accepted: ", out["findings"]["rows"][0]["fix"]["label"])
        self.assertEqual(out["accepted"], [{"scanner": "grype", "id": "CVE-1", "package": "*",
                                            "reason": "the vulnerable code is not compiled in", "matched": True}])

    def test_secrets_always_block(self):
        m = merged(env("gitleaks", [sr.finding(id="aws-key", severity="high", location="a.go:3")]))
        self.assertEqual(len(sr.violations(m, None, "push")), 1)

    def test_failed_and_missing_scanners_block_unless_optional(self):
        m = merged(env("osv-scanner", status="failed"), env("grype", status="not-run", target="x"),
                   env("grype", status="not-run", target="published", optional=True))
        self.assertEqual(len(sr.violations(m, None, "push")), 2)


class AcceptedFile(unittest.TestCase):
    def test_repository_file_parses_and_covers_the_opm_findings(self):
        path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "security-accepted.txt")
        accepted = sr.load_accepted(path)
        for ident in ("GO-2026-5064", "GO-2026-5338", "GO-2026-5622", "GO-2026-6791"):
            reason = accepted.get(("grype", ident, "github.com/containerd/containerd"), "")
            self.assertIn("go list -deps", reason, ident)
        # Matches no scanner finding: kept as a comment for the Dependabot dismissal.
        self.assertNotIn(("grype", "GHSA-685w-q87j-wqw3", "github.com/docker/distribution"), accepted)
        for key, reason in accepted.items():
            self.assertGreater(len(reason.split()), 5, "a reason must say how it was shown: %r" % (key,))


class Results(unittest.TestCase):
    """results.json is the Security page's view model: its honesty rules are tested here."""

    def test_schema_is_versioned(self):
        out = sr.build_results(merged(env("gitleaks")), None)
        self.assertEqual((out["schema"], out["kind"]), (sr.RESULTS_SCHEMA, "pgtoolbox.security-results"))
        self.assertEqual(out["envelopes"][0]["scanner"], "gitleaks")

    def test_not_run_is_never_zero(self):
        out = sr.build_results(merged(env("osv-scanner", status="not-run"), env("grype", status="failed", target="t")), None)
        rows = {r["scanner"]: r for r in out["scanners"]}
        self.assertEqual((rows["osv-scanner"]["status_label"], rows["osv-scanner"]["counts"], rows["osv-scanner"]["fixable"]),
                         ("not run", None, None))
        self.assertEqual((rows["grype"]["status_label"], rows["grype"]["counts"], rows["grype"]["reason"]), ("failed", None, "because"))
        self.assertEqual([m["status_label"] for m in out["missing"]], ["not run", "failed"])
        self.assertEqual((out["findings"]["state"], out["findings"]["ran"]), ("no-results", 0))
        self.assertEqual(out["images"][0]["status_label"], "failed")

    def test_empty_run_says_nothing_was_published(self):
        out = sr.build_results({"schema": 1, "envelopes": []}, None)
        self.assertFalse(out["published"])
        self.assertEqual(out["findings"]["state"], "no-results")

    def test_clean_run_states_its_scope(self):
        out = sr.build_results(merged(env("gitleaks"), env("gosec", status="not-run")), None)
        self.assertTrue(out["published"])
        self.assertEqual((out["findings"]["state"], out["findings"]["ran"]), ("none-open", 1))
        self.assertEqual(out["scanners"][0]["counts"], {"critical": 0, "high": 0, "medium": 0, "low": 0, "unknown": 0})

    def test_only_https_links_and_commit_ids_become_links(self):
        f = sr.finding(id="X", severity="high", package="p", fix="none", url="javascript:alert(1)")
        e = env("grype", [f], target="t")
        e["commit"] = "<img src=x>"
        out = sr.build_results({**merged(e), "commit": "0123abcd" * 5, "run_url": "http://evil"}, None)
        self.assertEqual(out["findings"]["rows"][0]["url"], "")
        self.assertEqual(out["scanners"][0]["commit_url"], "")
        self.assertEqual(out["run_url"], "")
        self.assertEqual(out["commit_url"], "https://github.com/fyannk/pgToolBox/commit/" + "0123abcd" * 5)

    def test_lower_severities_are_capped_but_every_high_is_listed(self):
        many = [vuln(f"CVE-L{i}", "low", "none") for i in range(sr.MAX_ROWS + 5)] + [vuln("CVE-H", "high", "fixed")]
        out = sr.build_results(merged(env("grype", many, target="t")), None)["findings"]
        self.assertEqual((out["total"], out["omitted"], out["rows"][0]["id"]), (sr.MAX_ROWS + 6, 5, "CVE-H"))
        self.assertEqual(out["rows"][0]["fix"], {"state": "fixed", "label": "yes: 2", "fixed_in": ["2"]})

    def test_fixable_is_only_counted_for_vulnerability_scanners(self):
        out = sr.build_results(merged(env("grype", [vuln("CVE-1", "high", "fixed")], target="t"),
                                      env("gosec", [sr.finding(id="G1", severity="high", location="a.go:1")])), None)
        rows = {r["scanner"]: r for r in out["scanners"]}
        self.assertEqual((rows["grype"]["fixable"], rows["gosec"]["fixable"]), (1, None))

    def test_missing_scorecard_reads_not_run(self):
        sc = sr.build_results(merged(env("gitleaks")), sr.scorecard_envelope("/nonexistent"))["scorecard"]
        self.assertEqual((sc["status_label"], sc["score"], sc["report_url"]), ("not run", None, ""))
        self.assertTrue(sc["reason"])
        self.assertEqual(sr.build_results(merged(), None)["scorecard"]["status_label"], "not run")

    def test_render_writes_results_json_only(self):
        with tempfile.TemporaryDirectory() as tmp:
            src = os.path.join(tmp, "merged.json")
            with open(src, "w", encoding="utf-8") as handle:
                json.dump(merged(env("gitleaks")), handle)
            out = os.path.join(tmp, "out")
            with contextlib.redirect_stdout(io.StringIO()):
                sr.main(["render", "--input", src, "--output-dir", out])
            self.assertEqual(os.listdir(out), ["results.json"])
            with open(os.path.join(out, "results.json"), encoding="utf-8") as handle:
                self.assertEqual(json.load(handle)["schema"], sr.RESULTS_SCHEMA)


class Readers(unittest.TestCase):
    def test_osv_severity_and_fix(self):
        doc = {"results": [{"source": {"path": "ui/pnpm-lock.yaml"}, "packages": [{
            "package": {"name": "left-pad", "version": "1.0.0"},
            "groups": [{"ids": ["GHSA-1"], "max_severity": "9.8"}],
            "vulnerabilities": [{"id": "GHSA-1", "summary": "bad", "affected": [{"package": {"name": "left-pad"},
                                 "ranges": [{"events": [{"introduced": "0"}, {"fixed": "1.0.1"}]}]}]}]}]}]}
        findings, _, _ = sr.read_osv(doc)
        self.assertEqual((findings[0]["severity"], findings[0]["fix"], findings[0]["fixed_in"]), ("critical", "fixed", ["1.0.1"]))

    def test_govulncheck_stream_keeps_only_called_symbols(self):
        text = (
            '{"config":{"scanner_name":"govulncheck"}}\n'
            '{"osv":{"id":"GO-1","summary":"bad"}}\n'
            '{"finding":{"osv":"GO-1","fixed_version":"v1.2.3","trace":[{"module":"example.com/m","version":"v1.0.0"}]}}\n'
            '{"finding":{"osv":"GO-1","fixed_version":"v1.2.3","trace":[{"module":"example.com/m","version":"v1.0.0","package":"p","function":"F"}]}}\n'
        )
        findings, _, _ = sr.read_govulncheck(sr.parse_json_stream(text))
        self.assertEqual(len(findings), 1)
        self.assertEqual((findings[0]["id"], findings[0]["fix"], findings[0]["fixed_in"]), ("GO-1", "fixed", ["v1.2.3"]))

    def test_sarif_severity_from_security_severity(self):
        doc = {"runs": [{"tool": {"driver": {"name": "t", "rules": [{"id": "r", "properties": {"security-severity": "7.5"}}]}},
                         "results": [{"ruleId": "r", "message": {"text": "m"}}]}]}
        self.assertEqual(sr.read_sarif(doc)[0][0]["severity"], "high")


class AcceptedByPath(unittest.TestCase):
    """A code-scanning acceptance names one file and covers nothing else."""

    def test_path_scoped(self):
        accepted = {("codeql", "go/weak", "a/b.go"): "reason"}
        env = {"scanner": "codeql"}
        hit = {"id": "go/weak", "package": "", "location": "a/b.go:12"}
        other = {"id": "go/weak", "package": "", "location": "a/c.go:3"}
        self.assertEqual(sr.accepted_reason(accepted, env, hit), "reason")
        self.assertEqual(sr.accepted_reason(accepted, env, other), "")


if __name__ == "__main__":
    unittest.main()

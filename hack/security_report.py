#!/usr/bin/env python3
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
"""Security results: normalise, merge, gate and publish.

The security workflow runs several scanners. Each one writes its native output
(grype JSON, SARIF, osv-scanner JSON); this script turns every output into one
"envelope" with the same fields, merges the envelopes of a run, decides whether
the run may pass, and writes results.json, the document the documentation
site's Security page reads.

    security_report.py normalize --scanner ID --kind KIND --input FILE --output FILE ...
    security_report.py unrun     --scanner ID --reason TEXT --output FILE ...
    security_report.py merge     --dir DIR --expect ID[:TARGET],... --output FILE
    security_report.py add       --input MERGED --envelope FILE --output MERGED
    security_report.py gate      --input FILE [--baseline FILE] [--event EVENT]
    security_report.py render    --input FILE --output-dir DIR [--scorecard FILE] [--accepted FILE]
                                 (writes DIR/results.json, schema 2: see RESULTS_SCHEMA)
    security_report.py summary   --input FILE

Honesty rule: a scanner that did not produce a result is "not run" (or
"failed") in every output. It is never counted as zero findings.

Standard library only, so it runs on a bare runner and in `make test-hack`.
"""

import argparse
import datetime
import json
import os
import re
import sys

SEVERITIES = ["critical", "high", "medium", "low", "unknown"]
RANK = {name: index for index, name in enumerate(SEVERITIES)}

# What each scanner is, and how the gate treats its findings.
#   fixable_critical_always : a critical with a fix available always fails
#   fixable_high_new        : a high with a fix available fails when it is not
#                             in the baseline (the latest run on main)
#   any                     : every finding fails (secrets, reachable
#                             vulnerabilities)
#   new_high                : a high or critical not in the baseline fails
#   report                  : shown, never fails (the scanner's own gate is
#                             elsewhere, or GitHub code scanning enforces it)
POLICY = {
    "grype": "vulnerability",
    "osv-scanner": "vulnerability",
    "govulncheck": "any",
    "gitleaks": "any",
    "gosec": "new_high",
    "zizmor": "new_high",
    "kube-linter": "new_high",
    "codeql": "report",
    "scorecard": "report",
}

TITLES = {
    "grype": "Grype: container images",
    "osv-scanner": "OSV-Scanner: npm and Go lockfiles",
    "govulncheck": "govulncheck: Go call graph",
    "gitleaks": "Gitleaks: secrets in history",
    "gosec": "gosec: Go static analysis",
    "zizmor": "zizmor: GitHub Actions workflows",
    "kube-linter": "kube-linter: Helm charts",
    "codeql": "CodeQL: code scanning",
    "scorecard": "OpenSSF Scorecard",
}


def now():
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def severity_from_score(score):
    """CVSS-style score (number or string) to a severity name."""
    try:
        value = float(score)
    except (TypeError, ValueError):
        return "unknown"
    if value >= 9.0:
        return "critical"
    if value >= 7.0:
        return "high"
    if value >= 4.0:
        return "medium"
    if value > 0:
        return "low"
    return "unknown"


def severity_from_name(name):
    name = (name or "").strip().lower()
    if name in ("negligible", "none", "info"):
        return "low"
    if name == "moderate":
        return "medium"
    return name if name in RANK else "unknown"


def run_context():
    server = os.environ.get("GITHUB_SERVER_URL", "https://github.com")
    repo = os.environ.get("GITHUB_REPOSITORY", "")
    run_id = os.environ.get("GITHUB_RUN_ID", "")
    return {
        "commit": os.environ.get("GITHUB_SHA", ""),
        "ref": os.environ.get("GITHUB_REF", ""),
        "event": os.environ.get("GITHUB_EVENT_NAME", ""),
        "run_url": f"{server}/{repo}/actions/runs/{run_id}" if repo and run_id else "",
    }


def finding(**kw):
    base = {
        "id": "",
        "severity": "unknown",
        "package": "",
        "version": "",
        "fixed_in": [],
        "fix": "n/a",  # fixed | none | unknown | n/a (a code or config finding)
        "location": "",
        "title": "",
        "url": "",
    }
    base.update(kw)
    return base


# ---------------------------------------------------------------- readers --

def read_grype(doc):
    findings = []
    for match in doc.get("matches", []):
        vuln = match.get("vulnerability", {})
        artifact = match.get("artifact", {})
        fix = vuln.get("fix", {}) or {}
        state = fix.get("state", "unknown")
        locations = artifact.get("locations") or []
        findings.append(finding(
            id=vuln.get("id", ""),
            severity=severity_from_name(vuln.get("severity")),
            package=artifact.get("name", ""),
            version=artifact.get("version", ""),
            fixed_in=list(fix.get("versions") or []),
            fix="fixed" if state == "fixed" else ("none" if state in ("not-fixed", "wont-fix") else "unknown"),
            location=(locations[0].get("path", "") if locations else ""),
            url=(vuln.get("dataSource") or ""),
        ))
    target = (doc.get("source") or {}).get("target") or {}
    subject = {}
    if isinstance(target, dict):
        subject = {
            "image": target.get("userInput", ""),
            "image_id": target.get("imageID", ""),
            "digest": target.get("manifestDigest", "") or next(iter(target.get("repoDigests") or []), ""),
        }
    descriptor = doc.get("descriptor", {})
    return findings, {"name": "grype", "version": descriptor.get("version", "")}, subject


def read_sarif(doc):
    findings = []
    tool = {"name": "", "version": ""}
    for run in doc.get("runs", []):
        driver = (run.get("tool") or {}).get("driver", {})
        tool = {"name": driver.get("name", tool["name"]), "version": driver.get("semanticVersion") or driver.get("version", tool["version"])}
        rules = {rule.get("id"): rule for rule in driver.get("rules", [])}
        for result in run.get("results", []):
            rule = rules.get(result.get("ruleId"), {})
            props = rule.get("properties", {}) or {}
            if "security-severity" in props:
                severity = severity_from_score(props["security-severity"])
            else:
                severity = {"error": "high", "warning": "medium", "note": "low"}.get(result.get("level", rule.get("defaultConfiguration", {}).get("level", "warning")), "unknown")
            location = ""
            locs = result.get("locations") or []
            if locs:
                physical = locs[0].get("physicalLocation", {})
                uri = (physical.get("artifactLocation") or {}).get("uri", "")
                line = (physical.get("region") or {}).get("startLine")
                location = f"{uri}:{line}" if line else uri
            message = (result.get("message") or {}).get("text", "")
            findings.append(finding(
                id=result.get("ruleId", ""),
                severity=severity,
                location=location,
                title=message.splitlines()[0][:200] if message else (rule.get("shortDescription") or {}).get("text", ""),
                url=rule.get("helpUri", ""),
            ))
    return findings, tool, {}


def read_osv(doc):
    findings = []
    for result in doc.get("results", []):
        source = (result.get("source") or {}).get("path", "")
        for pkg in result.get("packages", []):
            package = pkg.get("package", {})
            name = package.get("name", "")
            scores = {}
            for group in pkg.get("groups", []):
                for ident in group.get("ids", []):
                    scores[ident] = group.get("max_severity")
            for vuln in pkg.get("vulnerabilities", []):
                fixed = []
                for affected in vuln.get("affected", []):
                    if (affected.get("package") or {}).get("name", name) != name:
                        continue
                    for rng in affected.get("ranges", []):
                        for event in rng.get("events", []):
                            if "fixed" in event and event["fixed"] not in fixed:
                                fixed.append(event["fixed"])
                ident = vuln.get("id", "")
                findings.append(finding(
                    id=ident,
                    severity=severity_from_score(scores.get(ident)),
                    package=name,
                    version=package.get("version", ""),
                    fixed_in=fixed,
                    fix="fixed" if fixed else "none",
                    location=source,
                    title=vuln.get("summary", ""),
                    url=f"https://osv.dev/{ident}" if ident else "",
                ))
    return findings, {"name": "osv-scanner", "version": ""}, {}


def read_code_scanning_alerts(doc):
    findings = []
    for alert in doc if isinstance(doc, list) else []:
        rule = alert.get("rule", {})
        severity = severity_from_name(rule.get("security_severity_level"))
        if severity == "unknown":
            severity = {"error": "high", "warning": "medium", "note": "low"}.get(rule.get("severity", ""), "unknown")
        loc = (alert.get("most_recent_instance") or {}).get("location", {})
        findings.append(finding(
            id=rule.get("id", ""),
            severity=severity,
            location=f"{loc.get('path', '')}:{loc.get('start_line', '')}".rstrip(":"),
            title=rule.get("description", ""),
            url=alert.get("html_url", ""),
        ))
    return findings, {"name": "CodeQL", "version": ""}, {}


def parse_json_stream(text):
    """govulncheck -json prints a stream of JSON objects, not one document."""
    decoder = json.JSONDecoder()
    objects, index = [], 0
    while index < len(text):
        while index < len(text) and text[index].isspace():
            index += 1
        if index >= len(text):
            break
        obj, index = decoder.raw_decode(text, index)
        objects.append(obj)
    return objects


def read_govulncheck(objects):
    """Only findings whose trace starts at a function are reachable calls.

    govulncheck has no severity; a reachable known vulnerability is reported as
    high, and the gate treats every one of them as blocking regardless.
    """
    osvs, findings, seen = {}, [], set()
    for obj in objects:
        if "osv" in obj:
            osvs[obj["osv"]["id"]] = obj["osv"]
        elif "finding" in obj:
            item = obj["finding"]
            trace = item.get("trace") or []
            if not trace or not trace[0].get("function"):
                continue
            key = (item.get("osv", ""), trace[0].get("module", ""))
            if key in seen:
                continue
            seen.add(key)
            fixed = [item["fixed_version"]] if item.get("fixed_version") else []
            osv = osvs.get(item.get("osv", ""), {})
            findings.append(finding(
                id=item.get("osv", ""),
                severity="high",
                package=trace[0].get("module", ""),
                version=trace[0].get("version", ""),
                fixed_in=fixed,
                fix="fixed" if fixed else "none",
                location=f"{trace[0].get('package', '')}.{trace[0].get('function', '')}",
                title=osv.get("summary", ""),
                url=f"https://pkg.go.dev/vuln/{item.get('osv', '')}",
            ))
    return findings, {"name": "govulncheck", "version": ""}, {}


READERS = {
    "govulncheck-json": read_govulncheck,
    "grype": read_grype,
    "sarif": read_sarif,
    "osv-json": read_osv,
    "code-scanning-alerts": read_code_scanning_alerts,
}


# --------------------------------------------------------------- commands --

def envelope(scanner, status, reason="", **kw):
    env = {
        "schema": 1,
        "scanner": scanner,
        "title": TITLES.get(scanner, scanner),
        "target": "",
        "tool": {"name": scanner, "version": ""},
        "status": status,  # ok | failed | not-run
        "reason": reason,
        "scanned_at": now(),
        "subject": {},
        "findings": [],
    }
    env.update(run_context())
    env.update(kw)
    return env


def mise_version(key, path="mise.toml"):
    """The version mise.toml pins for KEY (keys are quoted, one per line)."""
    with open(path, encoding="utf-8") as handle:
        for line in handle:
            parts = line.split('"')
            if len(parts) >= 4 and parts[1] == key:
                return parts[3]
    return ""


def cmd_normalize(args):
    with open(args.input, encoding="utf-8") as handle:
        doc = parse_json_stream(handle.read()) if args.kind == "govulncheck-json" else json.load(handle)
    findings, tool, subject = READERS[args.kind](doc)
    if args.tool_name:
        tool["name"] = args.tool_name
    if args.tool_mise:
        tool["version"] = mise_version(args.tool_mise) or tool["version"]
    if args.tool_version:
        tool["version"] = args.tool_version
    extra = {}
    if args.scanned_at:
        extra["scanned_at"] = args.scanned_at
    if args.commit:
        extra["commit"] = args.commit
    env = envelope(args.scanner, "ok", target=args.target or "", tool=tool, subject=subject, findings=findings, **extra)
    for pair in args.subject or []:
        key, _, value = pair.partition("=")
        env["subject"][key] = value
    write_json(args.output, env)


def cmd_unrun(args):
    write_json(args.output, envelope(args.scanner, args.status, args.reason, target=args.target or "", optional=args.optional))


def cmd_merge(args):
    envelopes = []
    for root, _, names in os.walk(args.dir):
        for name in sorted(names):
            if name.endswith(".envelope.json"):
                with open(os.path.join(root, name), encoding="utf-8") as handle:
                    envelopes.append(json.load(handle))
    present = {(e["scanner"], e.get("target", "")) for e in envelopes}
    for item in filter(None, (args.expect or "").split(",")):
        scanner, _, target = item.partition(":")
        if (scanner, target) not in present:
            envelopes.append(envelope(scanner, "not-run", "the scanner job produced no result", target=target))
    envelopes.sort(key=lambda e: (e["scanner"], e.get("target", "")))
    ctx = run_context()
    write_json(args.output, {"schema": 1, "generated_at": now(), **ctx, "envelopes": envelopes})


def cmd_add(args):
    """Put one more envelope into a merged file, replacing the same scanner and target."""
    with open(args.input, encoding="utf-8") as handle:
        merged = json.load(handle)
    with open(args.envelope, encoding="utf-8") as handle:
        extra = json.load(handle)
    keep = [e for e in merged["envelopes"] if (e["scanner"], e.get("target", "")) != (extra["scanner"], extra.get("target", ""))]
    keep.append(extra)
    keep.sort(key=lambda e: (e["scanner"], e.get("target", "")))
    merged["envelopes"] = keep
    write_json(args.output, merged)


def finding_key(env, f):
    anchor = f["package"] or f["location"].rsplit(":", 1)[0]
    return (env["scanner"], env.get("target", ""), f["id"], anchor)


def load_baseline(path):
    keys = set()
    if not path or not os.path.exists(path):
        return None
    with open(path, encoding="utf-8") as handle:
        doc = json.load(handle)
    for env in doc.get("envelopes", []):
        if env.get("status") == "ok":
            keys.update(finding_key(env, f) for f in env["findings"])
    return keys


def load_accepted(path):
    """hack/security-accepted.txt: `scanner id package-or-path reason...`.

    The third field is the package for dependency scanners and the file path for
    code scanners (CodeQL, gosec), so an acceptance never covers more than the
    one place that was reviewed. `*` matches any package or path. A finding is
    accepted only when the reason shows it is unreachable or not a vulnerability
    where it stands. Accepted findings stay in the page, marked as such.
    """
    accepted = {}
    if not path or not os.path.exists(path):
        return accepted
    with open(path, encoding="utf-8") as handle:
        for line in handle:
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            fields = line.split(None, 3)
            if len(fields) == 4:
                accepted[(fields[0], fields[1], fields[2])] = fields[3]
    return accepted


def accepted_keys(f):
    """The package of a dependency finding, else the file of a code finding, then `*`."""
    path = f["location"].rsplit(":", 1)[0] if f.get("location") else ""
    return [k for k in (f["package"], path) if k] + ["*"]


def accepted_reason(accepted, env, f):
    for package in accepted_keys(f):
        reason = accepted.get((env["scanner"], f["id"], package))
        if reason:
            return reason
    return ""


def violations(merged, baseline, event, accepted=None):
    """Every reason the run must not pass, as readable lines."""
    accepted = accepted or {}
    out = []
    pull_request = event in ("pull_request", "pull_request_target")
    for env in merged["envelopes"]:
        name = env["scanner"] + (f" ({env['target']})" if env.get("target") else "")
        if env["status"] == "failed":
            out.append(f"{name}: the scanner failed ({env['reason']})")
            continue
        if env["status"] == "not-run":
            if not env.get("optional"):
                out.append(f"{name}: not run ({env['reason']})")
            continue
        policy = POLICY.get(env["scanner"], "report")
        for f in env["findings"]:
            if accepted_reason(accepted, env, f):
                continue
            sev = f["severity"]
            label = f"{name}: {sev} {f['id']} {f['package'] or f['location']}"
            is_new = baseline is None or finding_key(env, f) not in baseline
            if policy == "vulnerability":
                if sev == "critical" and f["fix"] == "fixed":
                    out.append(f"{label} (fixed in {', '.join(f['fixed_in']) or 'a newer version'}): a fixable critical blocks")
                elif sev == "high" and f["fix"] == "fixed" and pull_request and baseline is not None and is_new:
                    out.append(f"{label} (fixed in {', '.join(f['fixed_in']) or 'a newer version'}): a new fixable high blocks a pull request")
            elif policy == "any":
                out.append(f"{label}: {env['title']} reports this")
            elif policy == "new_high":
                if pull_request and baseline is not None and is_new and RANK[sev] <= RANK["high"]:
                    out.append(f"{label}: new {sev} finding")
    return out


def cmd_gate(args):
    with open(args.input, encoding="utf-8") as handle:
        merged = json.load(handle)
    baseline = load_baseline(args.baseline)
    if args.baseline and baseline is None:
        print(f"note: no baseline at {args.baseline}; highs are reported, only criticals and secrets block", file=sys.stderr)
    found = violations(merged, baseline, args.event, load_accepted(args.accepted))
    for line in found:
        print(f"::error::{line}")
    if found:
        print(f"security gate: {len(found)} blocking finding(s)", file=sys.stderr)
        return 1
    print("security gate: nothing blocks")
    return 0


def counts(env):
    out = {name: 0 for name in SEVERITIES}
    fixable = 0
    for f in env["findings"]:
        out[f["severity"]] += 1
        if f["fix"] == "fixed":
            fixable += 1
    return out, fixable


def cmd_summary(args):
    with open(args.input, encoding="utf-8") as handle:
        merged = json.load(handle)
    print("| Scanner | Tool | Status | Critical | High | Medium | Low | Fixable |")
    print("|---|---|---|---|---|---|---|---|")
    for env in merged["envelopes"]:
        name = env["title"] + (f" `{env['target']}`" if env.get("target") else "")
        tool = f"{env['tool']['name']} {env['tool']['version']}".strip()
        if env["status"] != "ok":
            print(f"| {name} | {tool} | {env['status']}: {env['reason']} | - | - | - | - | - |")
            continue
        c, fixable = counts(env)
        print(f"| {name} | {tool} | ok | {c['critical']} | {c['high']} | {c['medium']} | {c['low']} | {fixable} |")
    return 0


# ----------------------------------------------------------------- render --
#
# `render` writes results.json, the document the site's Security page
# (docs/site/src/pages/security.tsx) reads at runtime. Every decision the page
# shows is taken here: which scanners did not produce a result, the label of a
# status and of a fix, which findings are listed, which are accepted. The page
# only lays the document out, so the honesty rules live in one place and are
# tested by `make test-hack`.
#
# results.json, schema 2 (RESULTS_SCHEMA; bump it on any incompatible change,
# the page refuses a schema it does not know):
#
#   schema        2
#   kind          "pgtoolbox.security-results"
#   generated_at  when the security run was merged (UTC, RFC 3339)
#   commit, commit_url, ref, event, run_url   the run; a URL is "" when unknown
#   published     false when no scanner produced anything: the page then says
#                 "No security run has been published yet"
#   missing       [{name, status, status_label, reason}] scanners without a result
#   scanners      [{scanner, title, target, name, tool{name,version}, status,
#                   status_label, reason, scanned_at, commit, commit_url,
#                   counts{critical,high,medium,low,unknown} | null,
#                   fixable | null}]   counts is null unless the scanner ran;
#                   fixable is null for scanners without a notion of a fix
#   images        [{image, target, status, status_label, digest, image_id,
#                   commit, commit_url}]   the grype targets
#   scorecard     {status, status_label, reason, score, date, version, commit,
#                   commit_url, report_url, checks[{name, score, reason}]}
#   findings      {state: "no-results" | "none-open" | "listed", ran, total,
#                   omitted, rows[{severity, scanner, target, id, url, title,
#                   location, version, fix{state, label, fixed_in}}]}
#                   fix.state: fixed | none | unknown | n/a | accepted
#   accepted      [{scanner, id, package, reason, matched}] hack/security-accepted.txt
#   envelopes     the merged envelopes as they came, every finding included

RESULTS_SCHEMA = 2
MAX_ROWS = 400
REPO_URL = "https://github.com/fyannk/pgToolBox"
SCORECARD_URL = "https://scorecard.dev/viewer/?uri=github.com/fyannk/pgToolBox"
FIX_SCANNERS = ("grype", "osv-scanner")


def status_label(status):
    return {"ok": "ok", "not-run": "not run"}.get(status, "failed")


def commit_url(commit):
    """A link to the commit, only for something that is a commit id."""
    commit = commit or ""
    if re.fullmatch(r"[0-9a-f]{7,40}", commit):
        return f"{REPO_URL}/commit/{commit}"
    return ""


def safe_url(url):
    """Only https links reach the page; anything else is dropped."""
    url = url or ""
    return url if url.startswith("https://") else ""


def display_name(env):
    return env["title"] + (f" ({env['target']})" if env.get("target") else "")


def fix_of(accepted, env, f):
    reason = accepted_reason(accepted, env, f)
    if reason:
        return {"state": "accepted", "label": "accepted: " + reason, "fixed_in": f["fixed_in"]}
    if f["fix"] == "fixed":
        return {"state": "fixed", "label": "yes: " + (", ".join(f["fixed_in"]) or "newer version"), "fixed_in": f["fixed_in"]}
    if f["fix"] == "none":
        return {"state": "none", "label": "no fix yet (stays open)", "fixed_in": []}
    if f["fix"] == "unknown":
        return {"state": "unknown", "label": "unknown", "fixed_in": []}
    return {"state": "n/a", "label": "n/a (code or configuration)", "fixed_in": []}


def scanner_row(env):
    row = {
        "scanner": env["scanner"],
        "title": env["title"],
        "target": env.get("target", ""),
        "name": display_name(env),
        "tool": {"name": env["tool"].get("name", ""), "version": env["tool"].get("version", "")},
        "status": env["status"],
        "status_label": status_label(env["status"]),
        "reason": env.get("reason", ""),
        "scanned_at": env.get("scanned_at", ""),
        "commit": env.get("commit", ""),
        "commit_url": commit_url(env.get("commit", "")),
        "counts": None,
        "fixable": None,
    }
    if env["status"] == "ok":
        c, fixable = counts(env)
        row["counts"] = c
        row["fixable"] = fixable if env["scanner"] in FIX_SCANNERS else None
    return row


def scorecard_view(scorecard):
    sc = dict(scorecard or {"status": "not-run", "reason": "the Scorecard API has no result for this repository yet"})
    view = {
        "status": sc.get("status", "not-run"),
        "reason": sc.get("reason", ""),
        "score": sc.get("score"),
        "date": sc.get("date", ""),
        "version": sc.get("version", ""),
        "commit": sc.get("commit", ""),
        "commit_url": commit_url(sc.get("commit", "")),
        "report_url": SCORECARD_URL if sc.get("status") == "ok" else "",
        "checks": sc.get("checks", []),
    }
    view["status_label"] = status_label(view["status"])
    if view["status"] != "ok" and not view["reason"]:
        view["reason"] = "the Scorecard API has no result for this repository yet"
    return view


def build_results(merged, scorecard, accepted=None):
    """The view model of the Security page (results.json, schema 2)."""
    accepted = accepted or {}
    envs = merged.get("envelopes", [])
    ran = [e for e in envs if e["status"] == "ok"]

    rows = []
    for env in ran:
        if env["scanner"] == "scorecard":
            continue
        for f in env["findings"]:
            rows.append((RANK[f["severity"]], env, f))
    rows.sort(key=lambda r: (r[0], r[1]["scanner"], r[2]["id"], r[2]["package"]))
    # Every critical and high is listed; the lower severities are capped.
    shown = [r for r in rows if r[0] <= RANK["high"]] + [r for r in rows if r[0] > RANK["high"]][:MAX_ROWS]
    state = "no-results" if not ran else ("none-open" if not rows else "listed")

    matched = set()
    for env in ran:
        for f in env["findings"]:
            for package in accepted_keys(f):
                if (env["scanner"], f["id"], package) in accepted:
                    matched.add((env["scanner"], f["id"], package))

    return {
        "schema": RESULTS_SCHEMA,
        "kind": "pgtoolbox.security-results",
        "generated_at": merged.get("generated_at", ""),
        "commit": merged.get("commit", ""),
        "commit_url": commit_url(merged.get("commit", "")),
        "ref": merged.get("ref", ""),
        "event": merged.get("event", ""),
        "run_url": safe_url(merged.get("run_url", "")),
        "published": bool(envs),
        "missing": [
            {"name": display_name(e), "status": e["status"], "status_label": status_label(e["status"]), "reason": e.get("reason", "")}
            for e in envs if e["status"] != "ok"
        ],
        "scanners": [scanner_row(e) for e in envs],
        "images": [
            {
                "image": e.get("subject", {}).get("image") or e.get("target", ""),
                "target": e.get("target", ""),
                "status": e["status"],
                "status_label": status_label(e["status"]),
                "digest": e.get("subject", {}).get("digest", ""),
                "image_id": e.get("subject", {}).get("image_id", ""),
                "commit": e.get("commit", ""),
                "commit_url": commit_url(e.get("commit", "")),
            }
            for e in envs if e["scanner"] == "grype"
        ],
        "scorecard": scorecard_view(scorecard),
        "findings": {
            "state": state,
            "ran": len(ran),
            "total": len(rows),
            "omitted": len(rows) - len(shown),
            "rows": [
                {
                    "severity": f["severity"],
                    "scanner": env["scanner"],
                    "target": env.get("target", ""),
                    "id": f["id"],
                    "url": safe_url(f["url"]),
                    "title": f["title"],
                    "location": f["package"] or f["location"],
                    "version": f["version"],
                    "fix": fix_of(accepted, env, f),
                }
                for _, env, f in shown
            ],
        },
        "accepted": [
            {"scanner": s, "id": i, "package": p, "reason": r, "matched": (s, i, p) in matched}
            for (s, i, p), r in sorted(accepted.items())
        ],
        "envelopes": envs,
    }


def scorecard_envelope(path):
    """The Scorecard API answer, or a not-run description when it is missing."""
    if not path or not os.path.exists(path) or os.path.getsize(path) == 0:
        return {"status": "not-run", "reason": "the Scorecard API could not be read in this build"}
    try:
        with open(path, encoding="utf-8") as handle:
            doc = json.load(handle)
        return {
            "status": "ok",
            "score": doc["score"],
            "date": doc.get("date", ""),
            "version": (doc.get("scorecard") or {}).get("version", ""),
            "commit": (doc.get("repo") or {}).get("commit", ""),
            "checks": [{"name": c.get("name", ""), "score": c.get("score", ""), "reason": c.get("reason", "")} for c in doc.get("checks", [])],
        }
    except (ValueError, KeyError, TypeError):
        return {"status": "not-run", "reason": "the Scorecard API returned no score for this repository"}


def cmd_render(args):
    with open(args.input, encoding="utf-8") as handle:
        merged = json.load(handle)
    results = build_results(merged, scorecard_envelope(args.scorecard), load_accepted(args.accepted))
    os.makedirs(args.output_dir, exist_ok=True)
    # The page itself is the site's /security/ route (docs/site/src/pages/security.tsx);
    # writing an index.html here would replace it.
    write_json(os.path.join(args.output_dir, "results.json"), results)
    print(f"wrote {args.output_dir}/results.json (schema {RESULTS_SCHEMA}, {len(merged.get('envelopes', []))} scanner result(s))")
    return 0


def write_json(path, doc):
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(doc, handle, indent=2, sort_keys=True)
        handle.write("\n")


def build_parser():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("normalize")
    p.add_argument("--scanner", required=True)
    p.add_argument("--kind", required=True, choices=sorted(READERS))
    p.add_argument("--input", required=True)
    p.add_argument("--output", required=True)
    p.add_argument("--target")
    p.add_argument("--tool-name")
    p.add_argument("--tool-version")
    p.add_argument("--tool-mise", help="mise.toml key whose pinned version is the tool's version")
    p.add_argument("--scanned-at")
    p.add_argument("--commit")
    p.add_argument("--subject", action="append", help="KEY=VALUE added to the subject (image, digest)")
    p.set_defaults(func=cmd_normalize)

    p = sub.add_parser("unrun")
    p.add_argument("--scanner", required=True)
    p.add_argument("--reason", required=True)
    p.add_argument("--output", required=True)
    p.add_argument("--target")
    p.add_argument("--status", default="not-run", choices=["not-run", "failed"])
    p.add_argument("--optional", action="store_true", help="a legitimate skip (for example an image that is not published yet): the gate does not fail on it")
    p.set_defaults(func=cmd_unrun)

    p = sub.add_parser("merge")
    p.add_argument("--dir", required=True)
    p.add_argument("--output", required=True)
    p.add_argument("--expect", help="comma-separated scanner[:target] that must be present")
    p.set_defaults(func=cmd_merge)

    p = sub.add_parser("add")
    p.add_argument("--input", required=True)
    p.add_argument("--envelope", required=True)
    p.add_argument("--output", required=True)
    p.set_defaults(func=cmd_add)

    p = sub.add_parser("gate")
    p.add_argument("--input", required=True)
    p.add_argument("--baseline")
    p.add_argument("--accepted", help="hack/security-accepted.txt")
    p.add_argument("--event", default="")
    p.set_defaults(func=cmd_gate)

    p = sub.add_parser("summary")
    p.add_argument("--input", required=True)
    p.set_defaults(func=cmd_summary)

    p = sub.add_parser("render")
    p.add_argument("--input", required=True)
    p.add_argument("--output-dir", required=True)
    p.add_argument("--scorecard")
    p.add_argument("--accepted", help="hack/security-accepted.txt")
    p.set_defaults(func=cmd_render)
    return parser


def main(argv=None):
    args = build_parser().parse_args(argv)
    result = args.func(args)
    return result or 0


if __name__ == "__main__":
    sys.exit(main())

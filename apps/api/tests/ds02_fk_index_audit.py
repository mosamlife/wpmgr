#!/usr/bin/env python3
"""Static site/tenant delete FK audit; run from any directory, without Postgres."""

import argparse
import re
from pathlib import Path


LARGE = set("""
rum_events_raw agent_activity_log backup_file_index backup_manifest_entries
backup_chunks audit_log billing_events update_tasks backup_schedule_runs
restore_run_events site_uptime_probes agent_php_errors agent_login_events
site_connection_history site_events site_media_assets media_optimization_jobs
media_variant_results email_log
site_cache_hit_ratio_history cache_purge_audit rucss_results rucss_jobs
site_db_size_history font_transcode_results font_results rum_rollup_hourly
rum_rollup_daily site_email_log email_suppression
generated_reports site_object_cache_stats_history site_security_bans scan_runs
scan_run_hashes scan_findings site_file_baseline site_managed_files
site_vulnerabilities file_transfers site_content_inventory site_ability_inventory
org_context_versions site_context_versions assistant_update_proposals
assistant_cache_purge_requests assistant_ability_requests
""".split())


def clean(sql):
    # Keep quoted strings intact while removing comments and function bodies.
    pattern = r"--[^\n]*|/\*[\s\S]*?\*/|'(?:''|[^'])*'|\$(\w*)\$[\s\S]*?\$\1\$"
    return re.sub(pattern, lambda m: " " if m[0].startswith(("--", "/*", "$")) else m[0], sql)


def split_top(text, delimiter=","):
    parts, start, depth, quoted = [], 0, 0, False
    for i, char in enumerate(text):
        if char == "'":
            quoted = not quoted
        if quoted:
            continue
        if char == "(":
            depth += 1
        elif char == ")":
            depth -= 1
        elif char == delimiter and depth == 0:
            parts.append(text[start:i].strip())
            start = i + 1
    parts.append(text[start:].strip())
    return parts


def implied(predicate, cols):
    if not predicate:
        return True
    for term in predicate.split(" AND "):
        match = re.fullmatch(r"\(*\s*(\w+) IS NOT NULL\s*\)*", term)
        if not match or match[1] not in cols:
            return False
    return True


def audit(sql):
    sql = clean(sql)
    fks, indexes, tables = [], {}, set()
    bodies = []
    for match in re.finditer(r"^CREATE TABLE (?:IF NOT EXISTS )?(\w+)\s*\(([\s\S]*?)\);", sql, re.M):
        table, body = match.groups()
        tables.add(table)
        bodies.extend((table, item) for item in split_top(body))
    for match in re.finditer(r"^ALTER TABLE (\w+)\s+ADD ([\s\S]*?);", sql, re.M):
        bodies.append(match.groups())
    for table, item in bodies:
        ref = re.search(r"REFERENCES\s+(\w+)\s*\(([^)]+)\)[\s\S]*?ON DELETE (CASCADE|SET NULL)", item)
        if ref:
            fk = re.search(r"FOREIGN KEY\s*\(([^)]+)\)", item)
            cols = tuple(c.strip() for c in fk[1].split(",")) if fk else (item.split()[0],)
            explicit = re.search(r"CONSTRAINT\s+(\w+)\s+(?:FOREIGN KEY|REFERENCES)", item)
            name = explicit[1] if explicit else table + "_" + "_".join(cols) + "_fkey"
            fks.append((table, cols, ref[1], ref[2], ref[3], name))
        key = re.search(r"(?:PRIMARY KEY|UNIQUE)\s*\(([^)]+)\)", item)
        if key:
            cols = tuple(c.strip() for c in key[1].split(","))
            named = re.search(r"CONSTRAINT\s+(\w+)\s+(?:PRIMARY KEY|UNIQUE)", item)
            name = named[1] if named else table + ("_pkey" if "PRIMARY KEY" in item else "_" + "_".join(cols) + "_key")
            indexes.setdefault(table, []).append((cols, name, ""))
        elif re.search(r"\b(PRIMARY KEY|UNIQUE)\b", item) and not item.startswith("CONSTRAINT"):
            col = item.split()[0]
            name = table + ("_pkey" if "PRIMARY KEY" in item else "_" + col + "_key")
            indexes.setdefault(table, []).append(((col,), name, ""))
    for match in re.finditer(r"^CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:IF NOT EXISTS )?(\w+)\s+ON (\w+)(?: USING (\w+))?\s*\(([\s\S]*?)\)([\s\S]*?);", sql, re.M):
        name, table, method, keys, tail = match.groups()
        if method and method.lower() not in ("btree", "hash"):
            continue
        cols = tuple(re.sub(r"\s+(?:ASC|DESC|NULLS)[\s\S]*", "", c).strip() for c in split_top(keys))
        where = re.search(r"\bWHERE\s+([\s\S]*)", tail)
        indexes.setdefault(table, []).append((cols, name, " ".join(where[1].split()) if where else ""))
    if not tables or not fks:
        raise ValueError("schema contains no tables or delete foreign keys")
    deleting = {"sites", "tenants"}
    while True:
        reached = deleting | {fk[0] for fk in fks if fk[2] in deleting and fk[4] == "CASCADE"}
        if reached == deleting:
            break
        deleting = reached
    results = []
    for table, cols, parent, parent_cols, action, name in sorted(fks):
        if parent not in deleting:
            continue
        usable, conditional, prefixes = [], [], []
        for keys, index, predicate in indexes.get(table, []):
            if len(keys) < len(cols) or set(keys[:len(cols)]) != set(cols):
                if keys and keys[0] in cols and implied(predicate, cols):
                    prefixes.append(index + "(" + ",".join(keys) + ")" + (" [" + predicate + "]" if predicate else ""))
                continue
            # Equality to a parent key implies every referencing key is nonnull.
            if implied(predicate, cols):
                usable.append(index + (" [" + predicate + "]" if predicate else ""))
            else:
                conditional.append(index + " [" + predicate + "]")
        size = "large" if table in LARGE else ("moderate parent/token history" if table in {"backup_snapshots", "restore_runs", "update_runs", "autologin_tokens", "invitations"} else "small/bounded configuration")
        results.append((table, name, ",".join(cols), parent + "(" + parent_cols + ")", action,
                        "; ".join(usable) or "MISSING", "; ".join(conditional), size, "; ".join(prefixes)))
    return results


def self_test():
    fixture = """
CREATE TABLE tenants (id uuid PRIMARY KEY);
CREATE TABLE sites (id uuid PRIMARY KEY, tenant_id uuid REFERENCES tenants(id) ON DELETE CASCADE);
CREATE TABLE history (id uuid PRIMARY KEY, site_id uuid REFERENCES sites(id) ON DELETE CASCADE);
CREATE TABLE child (id uuid PRIMARY KEY, history_id uuid REFERENCES history(id) ON DELETE SET NULL);
CREATE TABLE untouched (child_id uuid REFERENCES child(id) ON DELETE CASCADE);
CREATE INDEX history_active ON history(site_id) WHERE state = 'active';
CREATE INDEX child_nonnull ON child(history_id) WHERE history_id IS NOT NULL;
ALTER TABLE history ADD CONSTRAINT history_pair UNIQUE (site_id, id);
"""
    rows = audit(fixture)
    assert {r[0] for r in rows} == {"sites", "history", "child"}, rows
    by_table = {r[0]: r for r in rows}
    assert by_table["sites"][5] == "MISSING", rows
    assert by_table["history"][5] == "history_pair", rows
    assert "history_active" in by_table["history"][6], rows
    assert "child_nonnull" in by_table["child"][5], rows
    assert audit(fixture.replace("ALTER TABLE history ADD CONSTRAINT history_pair UNIQUE (site_id, id);", ""))[1][5] == "MISSING"
    for sql in ("", "CREATE TABLE tenants (id uuid PRIMARY KEY);"):
        try:
            audit(sql)
        except ValueError:
            pass
        else:
            raise AssertionError("empty audit accepted")
    print("PASS DS-02 static audit self-test")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument("--missing-only", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        self_test()
    else:
        rows = audit((Path(__file__).parent.parent / "db/schema.sql").read_text())
        print("| Table | FK | Referencing columns | Parent | Action | Covering leading-key index | Conditional only | Likely size | Useful shorter prefix |")
        print("|---|---|---|---|---|---|---|---|---|")
        for row in rows:
            if not args.missing_only or row[5] == "MISSING":
                print("| " + " | ".join(row) + " |")
        print(f"\nAudited FKs={len(rows)}; affected child tables={len({r[0] for r in rows})}; missing leading-key indexes={sum(r[5] == 'MISSING' for r in rows)}")

# time-queries.py <queries.yaml> <container> <database>: three warm runs of
# each query as the exporter runs it (read only, its search_path, its
# predicate), in milliseconds.
import subprocess, sys, yaml, re
path, container, db = sys.argv[1], sys.argv[2], sys.argv[3]
qs = yaml.safe_load(open(path))
for name, q in qs.items():
    sql = "BEGIN READ ONLY;\nSET LOCAL search_path = pg_catalog, public, pg_temp;\n"
    if q.get("predicate_query"):
        sql += "SELECT (" + q["predicate_query"].strip() + ") AS predicate \\gset\n"
    sql += "\\timing on\n"
    for i in range(3):
        sql += "SELECT count(*) FROM (" + q["query"].strip() + ") x;\n"
    sql += "\\timing off\nCOMMIT;\n"
    p = subprocess.run(["docker","exec","-i",container,"psql","-X","-q","-U","cnpg_metrics_exporter","-d",db,"-At"], input=sql, capture_output=True, text=True)
    times = re.findall(r"Time: ([0-9.]+) ms", p.stdout)
    errs = [l for l in p.stderr.splitlines() if "ERROR" in l]
    rows = [l for l in p.stdout.splitlines() if l.isdigit()]
    print(f"{name:28s} rows={rows[-1] if rows else '-':>4s} ms={','.join(t.split('.')[0] for t in times)} {errs[:1]}")

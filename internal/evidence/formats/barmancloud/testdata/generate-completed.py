# Copyright 2026 The pgToolBox Authors.
# Licensed under the Apache License, Version 2.0.

import sys
from datetime import datetime, timezone

# Import output first to avoid Barman 3.19.1's Python 3.13 import cycle.
import barman.output  # noqa: F401
from barman.infofile import BackupInfo, Tablespace


backup = BackupInfo(
    "20260727T100000",
    server_name="alpha",
    status="DONE",
    mode="postgres",
    begin_time=datetime(2026, 7, 27, 10, 0, tzinfo=timezone.utc),
    end_time=datetime(2026, 7, 27, 10, 1, tzinfo=timezone.utc),
    begin_wal="000000010000000000000001",
    end_wal="000000010000000000000002",
    xlog_segment_size=16777216,
    size=42,
    deduplicated_size=21,
    compression="gzip",
    tablespaces=[
        Tablespace(
            "analytics",
            16384,
            "/var/lib/postgresql/tablespaces/analytics",
        )
    ],
)
backup.save(file_object=sys.stdout.buffer)

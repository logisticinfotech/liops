// SPDX-License-Identifier: Apache-2.0

import {
  Callout,
  Card,
  CardBody,
  CardHeader,
  Code,
  Divider,
  Grid,
  H1,
  H2,
  H3,
  Pill,
  Row,
  Stack,
  Stat,
  Table,
  Text,
  useHostTheme,
} from "cursor/canvas";

export default function LiopsCodebaseReview() {
  const theme = useHostTheme();

  return (
    <Stack gap={24}>
      <Stack gap={8}>
        <Row gap={8} align="center">
          <H1>liops codebase review</H1>
          <Pill tone="warning" active size="sm">
            9 P1
          </Pill>
        </Row>
        <Text tone="secondary">
          Whole-repo pass of the Go binary that backs up PostgreSQL and MySQL
          and monitors the production box. Read against PLAN.md, PLAN-mysql.md,
          PLAN-monitor.md, and the code in engines, jobs, HTTP, setup, and
          monitor. Source: repository review, 1 Nov 2026.
        </Text>
      </Stack>

      <Callout tone="warning">
        PostgreSQL backups match the design and are the production-ready path.
        There is no remote auth bypass: the UI is loopback-only, Host-checked,
        CSRF-protected, and it still never restores a live database. Do not
        trust MySQL point-in-time restore across a binlog reset until gap
        handling is fixed. Two local-UI paths go past the accepted “settings
        and dumps” threat model: the label-query Test runs arbitrary SQL as
        the postgres superuser, and the log viewer follows app-writable
        symlinks as root.
      </Callout>

      <Grid columns={4} gap={12}>
        <Stat value="0" label="Remote P0" tone="success" />
        <Stat value="9" label="P1 to fix first" tone="warning" />
        <Stat value="11" label="P2 / follow-ups" />
        <Stat value="~230" label="Go tests in repo" />
      </Grid>

      <H2>What this binary actually is</H2>
      <Text>
        One static Go process on the database server. Layer 1 is disaster
        recovery (pgBackRest WAL, or XtraBackup plus shipped binlogs). Layer 2
        is a nightly streamed dump per database. A separate non-root
        `liops-monitor` process samples the machine and answers JSON on a
        0600 Unix socket. The root UI is reached only through an SSH tunnel;
        there is no login, by design.
      </Text>

      <Grid columns={3} gap={12}>
        <Card>
          <CardHeader trailing={<Pill tone="success" active size="sm">Mature</Pill>}>
            PostgreSQL backups
          </CardHeader>
          <CardBody>
            <Text>
              pgBackRest writes layer 1. `pg_dump` stdout is piped into a
              multipart upload. Folder claiming, hex-encoded discovery, file
              job lock, and `restore-files` that only prints commands all
              match PLAN.md.
            </Text>
          </CardBody>
        </Card>
        <Card>
          <CardHeader trailing={<Pill tone="warning" active size="sm">PITR gaps</Pill>}>
            MySQL backups
          </CardHeader>
          <CardBody>
            <Text>
              XtraBackup streams into `backup.xbstream`, then `backup.json`
              last. Dumps and `restore-files` are careful. Binlog shipping
              does not treat `RESET BINARY LOGS` as a gap, and a failed
              catch-up full is not retried.
            </Text>
          </CardBody>
        </Card>
        <Card>
          <CardHeader trailing={<Pill tone="info" active size="sm">Built, noisy</Pill>}>
            Monitoring
          </CardHeader>
          <CardBody>
            <Text>
              Isolated user, no backup credentials, live SQL never stored.
              Verdict and Laravel checks can mislead: swap is in bytes/s
              judged as pages/s, steal is counted as busy CPU, and missing
              `.env` keys skip Laravel’s bad defaults.
            </Text>
          </CardBody>
        </Card>
      </Grid>

      <H2>What is already strong</H2>
      <Text>
        The architecture is the right one for a root process that holds
        credentials: plain Go, a short dependency list (minio-go, cron,
        go-mail, bbolt, yaml.v3), no web framework, and tests that replace
        programs, S3, and SMTP.
      </Text>
      <Table
        headers={["Invariant", "How it is held"]}
        columnAlign={["left", "left"]}
        rows={[
          [
            "UI never on the public net",
            "validateLoopback requires an IP with IsLoopback; ListenAndServe checks again; LocalOnly rejects non-localhost Host",
          ],
          [
            "No restore button",
            "No /restore route. UI shows filled-in commands. restore-files refuses the live datadir and only prepares an empty directory",
          ],
          [
            "Secrets stay off argv",
            "MySQL uses --defaults-extra-file. Redis AUTH is on stdin. pgBackRest keys live in a 0640 file, not in archive_command",
          ],
          [
            "Dumps never touch disk",
            "io.Pipe into multipart Put. Failed producer deletes the object. Keys are url.PathEscape’d under dumps/",
          ],
          [
            "One job at a time",
            "flock on job.lock so liops run queues behind serve. Binlogs use a separate lock. A broken state file does not abort a backup",
          ],
          [
            "Monitor cannot steal backups",
            "Own user, hardened unit, own bbolt file. POST /v1/push only accepts db.size:, disk.use:, fpm., opcache. Real SQL is live-only",
          ],
          [
            "Config is picky",
            "KnownFields(true), Load refuses group/other-readable files, Save is temp + fsync + rename at 0600",
          ],
        ]}
      />

      <H2>Fix these first</H2>
      <Text tone="secondary">
        Ranked by what an operator would actually lose: restore windows,
        then extra local privilege, then a misleading verdict.
      </Text>
      <Table
        headers={["ID", "Area", "Issue", "Impact", "Fix"]}
        columnAlign={["left", "left", "left", "left", "left"]}
        rowTone={[
          "danger",
          "danger",
          "warning",
          "warning",
          "warning",
          "warning",
          "warning",
          "warning",
          "warning",
        ]}
        rows={[
          [
            "P1.1",
            "MySQL PITR",
            "binlogRange only counts upward. RESET BINARY LOGS returns an empty list and no error.",
            "Shipping reports success. Dashboard looks healthy. Restore across the reset is a silent hole.",
            "Treat a lower current number, or a GTID set that does not extend, as a gap. Take a full. Compare indexes numerically.",
          ],
          [
            "P1.2",
            "MySQL PITR",
            "Gaps are stored and binlog_shipped_at is updated before the catch-up full runs. newGaps then suppresses the same names.",
            "If Run(Full) fails, later ships send no email and take no full. The gap is invisible.",
            "Persist the gap only after a successful full, or keep a catch-up-needed flag until one completes. Surface it in mysqlHealth.",
          ],
          [
            "P1.3",
            "Local UI",
            "Settings and wizard label Test run Labels SQL via psql -c as the postgres OS user (superuser). Multiple statements, including COPY … TO PROGRAM, are allowed.",
            "Anything that can open 127.0.0.1:8899 (SSRF, www-data) gets a shell as postgres, past “download dumps / change settings”.",
            "Allow one SELECT only. Reject ; COPY DO CALL SELECT INTO. Prefer a non-superuser role.",
          ],
          [
            "P1.4",
            "Local UI",
            "Log viewer globs Laravel storage/logs/*.log then os.Open, which follows symlinks. serve is root.",
            "An app user can plant steal.log → /etc/liops/config.yaml and read the bucket secret and SMTP password that Settings hides.",
            "Open with O_NOFOLLOW, or Lstat and require a regular file still under the glob after EvalSymlinks.",
          ],
          [
            "P1.5",
            "Verdict",
            "collector writes swap.in as bytes/s (pswpin × page size). verdict.Judge treats >= 10 as pages/s.",
            "One 4 KB page a minute already averages ~68, a Problem at weight 95. Daily mail can say “short of memory” on a quiet box.",
            "Judge bytes/s (threshold ~40 KiB/s) or stop multiplying by page size. Add a scenario test with collector units.",
          ],
          [
            "P1.6",
            "Verdict",
            "cpu.busy is 100 − idle − iowait, so steal is inside busy. Steal < 10% is a Warning; “outgrown the VM” is a Problem and is not suppressed.",
            "6–9% steal plus a busy-looking CPU headlines as “add CPUs”. PLAN says steal means move the VM; tuning will not help.",
            "Exclude steal from busy, or skip the “more CPUs” rule whenever steal >= 5%.",
          ],
          [
            "P1.7",
            "Redis",
            "Monitor ACL is nopass +slowlog|get. Comment says the user cannot read secrets. SLOWLOG holds real command arguments.",
            "Any local process can AUTH as liops-monitor with any password and read the slow log, even when requirepass is set.",
            "Give the user a password, or drop +slowlog|get. Keep CONFIG GET off.",
          ],
          [
            "P1.8",
            "Monitor store",
            "A minute caps routes at 2000, but each distinct host+route still gets a permanent bbolt id. pruneRequests never deletes bucketRoutes.",
            "A crawler can grow the id table until MemoryMax=100M restart-loops the monitor. PLAN §4.2 asked that random URLs not fill disk.",
            "Cap or expire route IDs. Collapse new ones to {other} after N. Prune unused ids.",
          ],
          [
            "P1.9",
            "Laravel",
            "QUEUE_CONNECTION and LOG_LEVEL are only flagged when the key is present and bad. Laravel defaults are sync and debug.",
            "An omitted line, the dangerous case, is silent. Telescope missing TELESCOPE_ENABLED is correctly treated as on.",
            "Treat missing QUEUE_CONNECTION / LOG_LEVEL in production as the framework default and flag them.",
          ],
        ]}
      />

      <H2>Follow-ups</H2>
      <Table
        headers={["ID", "Area", "Issue"]}
        rowTone={[
          "info",
          "info",
          "info",
          "info",
          "info",
          "info",
          "info",
          "info",
          "info",
          "info",
          "info",
        ]}
        rows={[
          [
            "P2.1",
            "Storage",
            "Prefixed.Probe calls the inner Probe, which writes liops-probe/<nano> at the bucket root. A failed read leaves the object. PLAN says everything lives under <backup name>/.",
          ],
          [
            "P2.2",
            "MySQL restore",
            "Banner uses server.timezone; printed --stop-datetime uses time.Local. On a UTC host with Asia/Kolkata config, “correcting” the command restores the wrong instant.",
          ],
          [
            "P2.3",
            "XtraBackup",
            "Version check runs only in the daily report and compares series (8.4 vs 8.4), not before each backup as PLAN-mysql §3.1 asked.",
          ],
          [
            "P2.4",
            "Dumps",
            "Deleted-database cleanup calls ClearDeleted even when Delete failed, so the object is “newly deleted” again next night and another email goes out.",
          ],
          [
            "P2.5",
            "Settings",
            "Each section save clones the whole config. Two overlapping saves last-write-win. WriteSettings can run before the YAML rename succeeds.",
          ],
          [
            "P2.6",
            "PostgreSQL",
            "POST /settings/monitor/restart and a failed wizard apply with restart=now do not re-check that a restart is pending. Tuning correctly stops short.",
          ],
          [
            "P2.7",
            "Jobs",
            "binlogs() does not defer unlock. Job context has no deadline, so a stuck SHOW BINARY LOGS or S3 Put holds the lock until shutdown.",
          ],
          [
            "P2.8",
            "Downloads",
            "Allowlist is strings.HasPrefix(dumps/). dumps/../pgbackrest/… passes the prefix; an exact List hit still has to exist, so this is unlikely to escape. Reject .. anyway.",
          ],
          [
            "P2.9",
            "Health UI",
            "Health holds healthMu for the whole check (up to 30s). Every non-wizard page waits on it.",
          ],
          [
            "P2.10",
            "Config",
            "postgres.cluster is only checked non-empty. filepath.Join cleans .., so a hostile cluster name can write conf.d outside /etc/postgresql.",
          ],
          [
            "P2.11",
            "Docs",
            "PLAN-mysql.md still says “nothing here is built yet”. PLAN-monitor.md still says “not started”. PLAN.md §10 is the accurate status.",
          ],
        ]}
      />

      <H2>Coverage against the plan</H2>
      <Grid columns={2} gap={16}>
        <Stack gap={8}>
          <H3>Implemented and consistent</H3>
          <Text>
            Streamed dumps, 16 MiB part size, hex-encoded discovery, dumps
            after backup, keep newest dump, folder prefix, owner markers,
            manifest last, file lock, CSRF + SameSite=Strict + CSP, no login,
            scheduler only after setup_complete, monitor isolation, verdict
            rules as a pure function, ApplyTuning limited to tuning.Allowed.
          </Text>
        </Stack>
        <Stack gap={8}>
          <H3>Missing or only partly there</H3>
          <Text>
            GTID ranges per shipped binlog (PLAN-mysql §3.2). XtraBackup
            check before every backup. Probe under the server folder. Charts
            are vendored uPlot, not server SVG. Monitor MemoryMax is 100M
            vs a 50 MB plan budget. Disk “bottleneck” fires at 60% util,
            plan said near 100%. work_mem is not charged to the shared RAM
            budget.
          </Text>
        </Stack>
      </Grid>

      <Divider />

      <H2>Tests that should exist and do not</H2>
      <Text>
        Pure retention, restore planning, CSRF, loopback bind, secret
        redaction, and the eleven verdict scenarios are in good shape. These
        production paths are not:
      </Text>
      <Table
        headers={["Path", "Why it matters"]}
        rows={[
          [
            "binlogRange when current < anchor; GTID discontinuity",
            "P1.1: the silent PITR hole",
          ],
          [
            "Catch-up Run(Full) fails, then the next ship",
            "P1.2: gap swallowed",
          ],
          [
            "Label Test rejects COPY TO PROGRAM and a second statement",
            "P1.3: superuser SQL",
          ],
          [
            "Log open of a symlink to config.yaml",
            "P1.4: root secret read",
          ],
          [
            "swap.in from collector fed into Judge",
            "P1.5: units never meet in one test",
          ],
          [
            "Steal ~6% plus high cpu.busy",
            "P1.6: wrong headline",
          ],
          [
            "S3.Probe failure and prefix",
            "Tests use storage.Memory only",
          ],
          [
            "Two concurrent settings section saves",
            "P2.5 last-write-wins",
          ],
        ]}
      />

      <H2>Suggested order of work</H2>
      <Stack gap={6}>
        <Text>
          1. MySQL gap handling (P1.1, P1.2) before any production MySQL
          PITR. Add the reset and failed-full tests first.
        </Text>
        <Text>
          2. Label-query allowlist and log O_NOFOLLOW (P1.3, P1.4). These
          are the two places the accepted local-UI trade-off becomes a
          postgres shell or a secret dump.
        </Text>
        <Text>
          3. Verdict units and steal (P1.5, P1.6) plus Laravel missing keys
          (P1.9). Wrong headlines train people to ignore the daily mail.
        </Text>
        <Text>
          4. Redis ACL password and route-id cap (P1.7, P1.8) so the
          monitor stays inside its memory budget and does not publish slow
          log arguments.
        </Text>
        <Text>
          5. Probe prefix, restore timezone label, settings merge, and
          refresh PLAN-mysql.md / PLAN-monitor.md status so the design docs
          stop saying the code is unbuilt.
        </Text>
        <Text>
          6. Keep the already-planned week on a real Ubuntu server. That is
          still the gate PLAN.md §10 named for Phase 1 and Phase 3.
        </Text>
      </Stack>

      <Text size="small" tone="tertiary" style={{ color: theme.text.tertiary }}>
        Threat model held as written: anything that can connect to
        127.0.0.1:8899 can use the UI. CSRF, SameSite, and the Host check
        stop other websites in the operator’s browser; they do not stop
        www-data on the same host. P1.3 and P1.4 are called out because they
        go past that baseline into a postgres shell and root-readable
        secrets.
      </Text>
    </Stack>
  );
}

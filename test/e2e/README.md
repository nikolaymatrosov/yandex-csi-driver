# E2E acceptance

Bash scripts that bring up a Talos cluster on Yandex Cloud, install the driver chart, run the acceptance checks and
delete everything. A full run takes 45–60 minutes and creates billable resources: 3 VMs, a few SSD disks, a 186 GB
non-replicated disk for a couple of minutes, a KMS key.

## Requirements

- Tools: `yc` (configured for the target folder), `talosctl` 1.14.x, `kubectl`, `helm` ≥ 3.8, `jq`, `curl`, `git`
  (for `kubectl apply -k` of the snapshot CRDs). Bash 3.2+ (macOS `/bin/bash` works). Check 10 also uses `csi-test`
  (`csi-nf-snap`).
- A Talos image in the folder (`TALOS_IMAGE`, default `talos-metal-yc-v1-14-6`), imported once from the Talos
  `metal` image of `TALOS_VERSION`.
- Folder permissions to create VMs, disks, addresses, security groups, subnets, service accounts, KMS keys and to
  assign folder roles.
- Network `default` with subnet `default-$ZONE`; in `ZONE2` either `default-$ZONE2` or a free `10.132.0.0/24`.
- Quotas. `ZONE` (default `ru-central1-a`): 4 vCPU, 8 GB RAM, 20 GB HDD, ~20 GB SSD, 2 static public IPs.
  `ZONE2` (default `ru-central1-e`): 2 vCPU, 4 GB RAM, 10 GB HDD, ~4 GB SSD and 186 GB non-replicated SSD
  (`compute.ssdNonReplicatedDisks.size`, 0 by default).
  Check 03 also creates 2 disk placement groups in `ZONE2` (`compute.diskPlacementGroups.count`, 5 per cloud) and
  holds one 93 GB non-replicated disk at a time.
- `E2E_PREFIX` must not match other resources in the folder: `down.sh` deletes everything named `$E2E_PREFIX` or
  `$E2E_PREFIX-*`.

## Settings

| Variable | Default | Meaning |
|---|---|---|
| `E2E_PREFIX` | `e2e` | prefix of all cloud resource names and of the work directory |
| `ZONE` / `ZONE2` | `ru-central1-a` / `ru-central1-e` | zones of cp+w1 / w2 |
| `E2E_VERSION` | `VERSION` file | driver version under test; the chart version is the same without `v` |
| `TALOS_VERSION` | `v1.14.6` | Talos version |
| `TALOS_IMAGE` | `talos-metal-yc-v1-14-6` | Talos image name in the folder |
| `TALOS_INSTALLER` | `factory.talos.dev/metal-installer/b68af66f…` | installer image, tagged with `TALOS_VERSION` |
| `KEEP` | empty | `1`: `run.sh` leaves the cluster running |

The folder is the current `yc` profile's (`yc config get folder-id`); switch profiles to use another folder.

## Commands

```bash
test/e2e/run.sh                 # up, all checks, down; exit code 0 only if all checks pass and nothing is left behind
KEEP=1 test/e2e/run.sh          # same, but keep the cluster for debugging
test/e2e/checks.sh 02 08        # re-run selected checks on a kept cluster
test/e2e/down.sh                # delete everything (safe to repeat, works without state)
bash test/e2e/lib_test.sh       # offline self-check of the helpers
```

Checks run in order and depend on each other: `csi-test` from 02 is used by 04, 05 and 09; `csi-snap-a` from 04 by 09.

## Re-running checks

Checks are written for one ordered pass. On a kept cluster 04 after 05 fails (`csi-test` is then 8Gi, the restore asks
for 4Gi), 09 needs `csi-snap-a` from 04, and 08 fails if `yc-network-ssd-kms` is left from an aborted run
(StorageClass parameters are immutable; delete it first). A previous `results.md` is kept as `results.md.<timestamp>`.

Check 03 reuses placement groups left from an aborted run (found by name `$E2E_PREFIX-pg-*`) and recreates its
`yc-nrd-pg-*` StorageClasses with `kubectl apply`; if the group IDs changed, delete those StorageClasses first
(parameters are immutable). `down.sh` deletes the groups after the disks.

## Results

Everything goes to `test/e2e/_work/$E2E_PREFIX/` (git-ignored): `results.md` (table of checks with status PASS, FAIL,
SKIP or INFO and a summary line), `check-NN.log` (output of each check), `logs/` (driver container logs), plus the
Talos configs, kubeconfig, service account keys and `state.env` of the running cluster.

INFO rows are observations, not assertions (move time, cloud error texts, KMS restore behaviour). Check 09 is a race
and reports SKIP when the disk became READY before the controller restart.

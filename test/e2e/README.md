# E2E acceptance

Bash scripts that bring up a Talos cluster on Yandex Cloud, install the driver chart, run the acceptance checks and
delete everything. A full run takes 45–60 minutes and creates billable resources: 3 VMs, a few SSD disks, a 186 GB
non-replicated disk for a couple of minutes, a KMS key.

## Requirements

- Tools: `yc` (configured for the target folder), `talosctl` 1.14.x, `kubectl`, `helm` ≥ 3.8, `jq`, `curl`, `git`
  (for `kubectl apply -k` of the snapshot CRDs). Bash 3.2+ (macOS `/bin/bash` works).
- A Talos image in the folder (`TALOS_IMAGE`, default `talos-metal-yc-v1-14-6`), imported once from the Talos
  `metal` image of `TALOS_VERSION`.
- Folder permissions to create VMs, disks, addresses, security groups, subnets, service accounts, KMS keys and to
  assign folder roles.
- Quotas. `ZONE` (default `ru-central1-a`): 4 vCPU, 8 GB RAM, 20 GB HDD, ~16 GB SSD, 2 static public IPs.
  `ZONE2` (default `ru-central1-e`): 2 vCPU, 4 GB RAM, 10 GB HDD, ~4 GB SSD and 186 GB non-replicated SSD
  (`compute.ssdNonReplicatedDisks.size`, 0 by default).
- `E2E_PREFIX` must not match other resources in the folder: `down.sh` deletes everything named `$E2E_PREFIX` or
  `$E2E_PREFIX-*`.

## Settings

| Variable | Default | Meaning |
|---|---|---|
| `E2E_PREFIX` | `e2e` | prefix of all cloud resource names and of the work directory |
| `FOLDER_ID` | `yc config get folder-id` | folder |
| `ZONE` / `ZONE2` | `ru-central1-a` / `ru-central1-e` | zones of cp+w1 / w2 |
| `E2E_VERSION` | `VERSION` file | driver version under test; the chart version is the same without `v` |
| `TALOS_VERSION` | `v1.14.6` | Talos version |
| `TALOS_IMAGE` | `talos-metal-yc-v1-14-6` | Talos image name in the folder |
| `TALOS_INSTALLER` | `factory.talos.dev/metal-installer/b68af66f…` | installer image, tagged with `TALOS_VERSION` |
| `KEEP` | empty | `1`: `run.sh` leaves the cluster running |

## Commands

```bash
test/e2e/run.sh                 # up, all checks, down; exit code 0 only if all checks pass and nothing is left behind
KEEP=1 test/e2e/run.sh          # same, but keep the cluster for debugging
test/e2e/checks.sh 02 08        # re-run selected checks on a kept cluster
test/e2e/down.sh                # delete everything (safe to repeat, works without state)
bash test/e2e/lib_test.sh       # offline self-check of the helpers
```

Checks run in order and depend on each other: `csi-test` from 02 is used by 04, 05 and 09; `csi-snap-a` from 04 by 09.

## Results

Everything goes to `test/e2e/_work/$E2E_PREFIX/` (git-ignored): `results.md` (table of checks with status PASS, FAIL,
SKIP or INFO and a summary line), `check-NN.log` (output of each check), `logs/` (driver container logs), plus the
Talos configs, kubeconfig, service account keys and `state.env` of the running cluster.

INFO rows are observations, not assertions (move time, cloud error texts, KMS restore behaviour). Check 09 is a race
and reports SKIP when the disk became READY before the controller restart.

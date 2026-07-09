# Reset the `Onboarding` HAOS VM (VirtualBox)

How to wipe the team's local **Home Assistant OS** VirtualBox VM back to stock so
`smart-onboard` can run against a genuinely fresh unit. Use this when:

- Someone clicked through the HA browser onboarding wizard (CLI expects a clean
  wizard).
- A previous onboarding run left owner credentials, add-on state, or provisioning
  data on the disk.
- `smart-onboard` reports *"owner already exists"* on a unit that should be fresh.

The production runbook assumes a clean unit in
[`production-onboarding.md`](production-onboarding.md#1-prepare-the-unit). This
doc is the VirtualBox-specific re-flash.

---

## What the VM actually stores state on

Our `Onboarding` VM is registered in VirtualBox and keeps its config under
`~/VirtualBox/Onboarding/`. The **real** HAOS disk is **not** the small
`Onboarding/Onboarding.vdi` file — the VM boots from the shared image:

| Path | Role |
| ---- | ---- |
| `~/VirtualBox/haos_generic-aarch64-18.1.vdi` | HAOS system disk (this is what gets dirty) |
| `~/VirtualBox/haos_generic-aarch64-18.1.vdi.zip` | Pristine backup to restore from |
| `~/VirtualBox/Onboarding/Onboarding.vbox` | VM definition (CPU, RAM, NAT port forward) |
| `~/VirtualBox/Onboarding/Onboarding.nvram` | UEFI NVRAM (boot order, etc.) |

NAT forwards host `127.0.0.1:8123` → guest `:8123`, so from the Mac you reach HA
at `http://127.0.0.1:8123` (and `smart-onboard --dev` can find it on loopback).

Inspect the live setup:

```bash
VBoxManage list vms
VBoxManage showvminfo "Onboarding" | grep -E '^(Name|State|Config file|Location:)'
VBoxManage snapshot "Onboarding" list
```

---

## Fresh VM — restore the disk from zip (recommended)

Use this when there is **no** useful snapshot, or you want a full re-extract from
the known-good zip.

```bash
# 1. Power off the VM
VBoxManage controlvm "Onboarding" poweroff

# 2. Wait until fully stopped (State: powered off)
VBoxManage showvminfo "Onboarding" | grep '^State:'

# 3. Replace the dirty disk with a fresh extract
cd ~/VirtualBox
mv haos_generic-aarch64-18.1.vdi haos_generic-aarch64-18.1.vdi.dirty
unzip -o haos_generic-aarch64-18.1.vdi.zip

# 4. Start again
VBoxManage startvm "Onboarding"
```

**After start:** first boot downloads Home Assistant Core and can take several
minutes. Leave the unit alone — do **not** click through the HA onboarding
wizard in the browser. Hand off to `smart-onboard` per
[`production-onboarding.md`](production-onboarding.md).

Once you've confirmed a clean boot, delete the backup:

```bash
rm ~/VirtualBox/haos_generic-aarch64-18.1.vdi.dirty
```

---

## Faster reset — restore a snapshot (optional)

After you have a **genuinely fresh** HAOS once (Core up, onboarding screen
visible, nothing claimed), take a snapshot so future resets are one command:

```bash
VBoxManage controlvm "Onboarding" poweroff
VBoxManage snapshot "Onboarding" take "fresh-haos" \
  --description "Stock HAOS before smart-onboard"
```

Later:

```bash
VBoxManage controlvm "Onboarding" poweroff
VBoxManage snapshot "Onboarding" restore "fresh-haos"
VBoxManage startvm "Onboarding"
```

> **When to prefer zip over snapshot:** if Core/OS was upgraded on the VM, add-on
> data changed, or you're unsure the snapshot is still stock — re-extract from
> `haos_generic-aarch64-18.1.vdi.zip` instead.

---

## Nuclear option — delete and recreate the VM

Only if the VM definition itself is broken. You still need the zip (or a clean
`.vdi`) to attach as the hard disk.

```bash
VBoxManage controlvm "Onboarding" poweroff 2>/dev/null || true
VBoxManage unregistervm "Onboarding" --delete   # removes ~/VirtualBox/Onboarding/

cd ~/VirtualBox
unzip -o haos_generic-aarch64-18.1.vdi.zip      # if .vdi is missing

# Recreate with the same settings the team uses (ARM HAOS, NAT :8123 forward).
# Easiest path: re-import from your saved VM export, or clone settings from a
# colleague's Onboarding.vbox. Document any local tweaks here if the team changes
# the template.
```

---

## Verify the unit is fresh

Before running `smart-onboard`:

1. Open `http://127.0.0.1:8123` — you should see the **HA onboarding** screen, not
   a login for an owner you didn't create in this session.
2. Do **not** complete the browser wizard; the CLI creates the owner.
3. From the VM shell (Settings → Terminal), confirm outbound access if onboarding
   production (`production-onboarding.md` §1).

If `smart-onboard` still says *"owner already exists"*, the disk wasn't reset —
repeat the zip restore (or restore the `fresh-haos` snapshot) and avoid opening
the wizard in the browser until the CLI run finishes.

---

## Related docs

- [`production-onboarding.md`](production-onboarding.md) — onboard the fresh VM
  against `smart.prameg.net` + `mqtt.prameg.net`.
- [`local-testing.md`](local-testing.md) — dev/testing against the same VM with
  `--dev` and a local broker.
- [`factory-onboarding.md`](factory-onboarding.md) — `smart-onboard` behavior and
  troubleshooting (*"owner already exists"*, NAT vs bridged networking).

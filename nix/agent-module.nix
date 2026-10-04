{ config, lib, pkgs, ... }:

let
  cfg = config.services.homelab-status-agent;
  exporterCfg = config.services.prometheus.exporters.node;
in
{
  options.services.homelab-status-agent = {
    enable = lib.mkEnableOption "homelab-status monitoring agent";

    basicAuthPasswordFile = lib.mkOption {
      type = lib.types.path;
      description = "Path to file containing Basic Auth password for node-exporter.";
    };

    listenPort = lib.mkOption {
      type = lib.types.port;
      default = 9100;
      description = "Port for prometheus-node-exporter to listen on.";
    };

    textfileDir = lib.mkOption {
      type = lib.types.path;
      default = "/var/lib/prometheus-node-exporter/textfile";
      description = "Directory for textfile collector .prom files.";
    };

    customChecks = {
      smart = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Enable SMART disk health monitoring via textfile collector.";
      };

      borgJobs = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [];
        description = "List of borgmatic job names to monitor.";
      };

      pikaBackupUsers = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [];
        description = "List of users running Pika Backup. The agent will read their history.json to report backup status.";
      };
    };
  };

  config = lib.mkIf cfg.enable (lib.mkMerge [
    {
      # Ensure textfile directory exists.
      systemd.tmpfiles.rules = [
        "d ${cfg.textfileDir} 0755 root root -"
      ];

      # Node Exporter with Basic Auth and systemd collector.
      services.prometheus.exporters.node = {
        enable = true;
        port = cfg.listenPort;
        enabledCollectors = [
          "systemd"
          "textfile"
        ];
        extraFlags = [
          "--collector.textfile.directory=${cfg.textfileDir}"
          # Without this flag the generated web config below is never read
          # and node-exporter serves metrics unauthenticated.
          "--web.config.file=/run/prometheus-node-exporter/web-config.yml"
          # Note: tune --collector.systemd.unit-include later to limit scope.
        ];
      };

      # Generate Basic Auth web config for node-exporter.
      # The password file is expected to contain a bcrypt hash.
      systemd.services.prometheus-node-exporter.serviceConfig.ExecStartPre =
        let
          webConfigFile = pkgs.writeText "web-config.yml" ''
            basic_auth_users:
              metrics: PLACEHOLDER
          '';
          script = pkgs.writeShellScript "gen-node-exporter-auth" ''
            PASS=$(cat ${cfg.basicAuthPasswordFile})
            ${pkgs.gnused}/bin/sed "s|PLACEHOLDER|$PASS|" ${webConfigFile} > /run/prometheus-node-exporter/web-config.yml
            # This runs as root, but node-exporter must be able to read the file.
            ${pkgs.coreutils}/bin/chown ${exporterCfg.user}:${exporterCfg.group} /run/prometheus-node-exporter/web-config.yml
            ${pkgs.coreutils}/bin/chmod 0400 /run/prometheus-node-exporter/web-config.yml
          '';
        in
        [
          "+${pkgs.coreutils}/bin/mkdir -p /run/prometheus-node-exporter"
          "+${script}"
        ];

      # SMART health textfile script.
      systemd.services.homelab-status-smart-check = lib.mkIf cfg.customChecks.smart {
        description = "Dump SMART health to node-exporter textfile";
        serviceConfig = {
          Type = "oneshot";
          ExecStart = pkgs.writeShellScript "smart-check" ''
            OUTPUT="${cfg.textfileDir}/smart.prom"
            echo "# HELP node_smart_healthy SMART health status per disk (1=healthy, 0=failing)" > "$OUTPUT.tmp"
            echo "# TYPE node_smart_healthy gauge" >> "$OUTPUT.tmp"
            for disk in $(${pkgs.util-linux}/bin/lsblk -dnp -o NAME,TYPE | ${pkgs.gawk}/bin/awk '$2=="disk"{print $1}'); do
              output=$(${pkgs.smartmontools}/bin/smartctl -H "$disk" 2>&1) || true
              # Skip devices smartctl cannot interrogate (USB bridges, etc.).
              if echo "$output" | ${pkgs.gnugrep}/bin/grep -qE "Unknown USB bridge|Please specify device type|Unable to detect device type"; then
                continue
              fi
              # ATA/NVMe report "PASSED"; SCSI/SAS/virtual disks report "SMART Health Status: OK".
              if echo "$output" | ${pkgs.gnugrep}/bin/grep -qE "PASSED|SMART Health Status: OK"; then
                echo "node_smart_healthy{disk=\"$disk\"} 1" >> "$OUTPUT.tmp"
              else
                echo "node_smart_healthy{disk=\"$disk\"} 0" >> "$OUTPUT.tmp"
              fi
            done
            mv "$OUTPUT.tmp" "$OUTPUT"
          '';
        };
      };

      systemd.timers.homelab-status-smart-check = lib.mkIf cfg.customChecks.smart {
        wantedBy = [ "timers.target" ];
        timerConfig = {
          OnBootSec = "2min";
          OnUnitActiveSec = "5min";
        };
      };

      # NixOS generation + reboot-required textfile script.
      systemd.services.homelab-status-nixos-info = {
        description = "Dump NixOS generation info to node-exporter textfile";
        serviceConfig = {
          Type = "oneshot";
          ExecStart = pkgs.writeShellScript "nixos-info" ''
            OUTPUT="${cfg.textfileDir}/nixos.prom"
            echo "# HELP node_nixos_generation Current NixOS system generation number" > "$OUTPUT.tmp"
            echo "# TYPE node_nixos_generation gauge" >> "$OUTPUT.tmp"
            gen=$(readlink /nix/var/nix/profiles/system | ${pkgs.gnugrep}/bin/grep -oP '\d+' | tail -1)
            echo "node_nixos_generation ''${gen:-0}" >> "$OUTPUT.tmp"

            echo "# HELP node_reboot_required Whether a reboot is needed (1=yes, 0=no)" >> "$OUTPUT.tmp"
            echo "# TYPE node_reboot_required gauge" >> "$OUTPUT.tmp"
            current=$(readlink /run/current-system)
            installed=$(readlink /nix/var/nix/profiles/system)
            if [ "$current" != "$installed" ]; then
              echo "node_reboot_required 1" >> "$OUTPUT.tmp"
            else
              echo "node_reboot_required 0" >> "$OUTPUT.tmp"
            fi
            mv "$OUTPUT.tmp" "$OUTPUT"
          '';
        };
      };

      systemd.timers.homelab-status-nixos-info = {
        wantedBy = [ "timers.target" ];
        timerConfig = {
          OnBootSec = "1min";
          OnUnitActiveSec = "5min";
        };
      };
    }

    # Borg backup status textfile scripts (one per configured job).
    {
      systemd.services = (lib.listToAttrs (map (job: {
        name = "homelab-status-borg-${job}";
        value = {
          description = "Dump Borg backup status for job ${job} to node-exporter textfile";
          # Run after the borg backup service if it exists, and share its
          # PATH so the borg-job-<name> wrapper (with BORG_REPO and
          # passphrase pre-configured) is available.
          path = [ "/run/current-system/sw" ];
          serviceConfig = {
            Type = "oneshot";
            ExecStart = pkgs.writeShellScript "borg-check-${job}" ''
              OUTPUT="${cfg.textfileDir}/borg_${job}.prom"
              echo "# HELP node_borg_last_backup_timestamp_seconds Unix timestamp of last successful Borg backup" > "$OUTPUT.tmp"
              echo "# TYPE node_borg_last_backup_timestamp_seconds gauge" >> "$OUTPUT.tmp"
              # Use the NixOS borg-job wrapper which has BORG_REPO and
              # passphrase pre-configured. Falls back to plain borg if
              # the wrapper does not exist.
              BORG="borg-job-${job}"
              if ! command -v "$BORG" >/dev/null 2>&1; then
                BORG="${pkgs.borgbackup}/bin/borg"
              fi
              ts=$($BORG list --last 1 --format '{time:%s}' 2>/dev/null || echo "0")
              echo "node_borg_last_backup_timestamp_seconds{job=\"${job}\"} $ts" >> "$OUTPUT.tmp"
              mv "$OUTPUT.tmp" "$OUTPUT"
            '';
          };
        };
      }) cfg.customChecks.borgJobs)) // (lib.listToAttrs (map (user: {
        name = "homelab-status-pika-${user}";
        value = {
          description = "Dump Pika Backup status for user ${user} to node-exporter textfile";
          serviceConfig = {
            Type = "oneshot";
            User = user;
            ExecStart = pkgs.writeShellScript "pika-check-${user}" ''
              OUTPUT="/tmp/pika_${user}.prom"
              echo "# HELP node_borg_last_backup_timestamp_seconds Unix timestamp of last successful Borg backup" > "$OUTPUT.tmp"
              echo "# TYPE node_borg_last_backup_timestamp_seconds gauge" >> "$OUTPUT.tmp"

              # Find the history file (could be Flatpak or Native)
              HOME_DIR=$(${pkgs.getent}/bin/getent passwd "${user}" | ${pkgs.coreutils}/bin/cut -d: -f6)
              FLATPAK_FILE="$HOME_DIR/.var/app/org.gnome.World.PikaBackup/config/pika-backup/history.json"
              NATIVE_FILE="$HOME_DIR/.config/pika-backup/history.json"

              FILE=""
              if [ -f "$FLATPAK_FILE" ]; then FILE="$FLATPAK_FILE"; fi
              if [ -f "$NATIVE_FILE" ]; then FILE="$NATIVE_FILE"; fi

              if [ -n "$FILE" ]; then
                for id in $(${pkgs.jq}/bin/jq -r 'keys[]' "$FILE" 2>/dev/null || echo ""); do
                  ts_iso=$(${pkgs.jq}/bin/jq -r ".\"$id\".last_completed.end // empty" "$FILE" 2>/dev/null || echo "")
                  if [ -n "$ts_iso" ]; then
                    ts_epoch=$(${pkgs.coreutils}/bin/date -d "$ts_iso" +%s 2>/dev/null || echo "0")
                    echo "node_borg_last_backup_timestamp_seconds{job=\"pika-${user}-$id\"} $ts_epoch" >> "$OUTPUT.tmp"
                  fi
                done
              fi

              mv "$OUTPUT.tmp" "$OUTPUT"
            '';
            ExecStartPost = "+${pkgs.coreutils}/bin/mv /tmp/pika_${user}.prom ${cfg.textfileDir}/pika_${user}.prom";
          };
        };
      }) cfg.customChecks.pikaBackupUsers));

      systemd.timers = (lib.listToAttrs (map (job: {
        name = "homelab-status-borg-${job}";
        value = {
          wantedBy = [ "timers.target" ];
          timerConfig = {
            OnBootSec = "5min";
            OnUnitActiveSec = "1h";
          };
        };
      }) cfg.customChecks.borgJobs)) // (lib.listToAttrs (map (user: {
        name = "homelab-status-pika-${user}";
        value = {
          description = "Timer for Pika Backup status check for user ${user}";
          wantedBy = [ "timers.target" ];
          timerConfig = {
            OnBootSec = "5min";
            OnUnitActiveSec = "1h";
          };
        };
      }) cfg.customChecks.pikaBackupUsers));
    }
  ]);
}

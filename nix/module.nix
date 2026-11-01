self:
{ config, lib, pkgs, ... }:

let
  cfg = config.services.timew-api;
in
{
  options.services.timew-api = {
    enable = lib.mkEnableOption "timew-api, an HTTP API and web page for timewarrior";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "timew-api.packages.\${system}.default";
    };

    port = lib.mkOption {
      type = lib.types.port;
      default = 8097;
      description = "Port on 127.0.0.1. Only cloudflared should reach it.";
    };

    dataDir = lib.mkOption {
      type = lib.types.path;
      default = "/var/lib/timew-api";
      description = "The timewarrior database (TIMEWARRIORDB). Put it somewhere that is backed up.";
    };

    environmentFile = lib.mkOption {
      type = lib.types.path;
      description = ''
        Not in the Nix store. Must set TIMEW_API_CF_TEAM_DOMAIN and
        TIMEW_API_CF_AUD (the Access application's AUD tag); may set
        TIMEW_API_PROJECT (the weekly report's label).
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    users.users.timew-api = { isSystemUser = true; group = "timew-api"; home = cfg.dataDir; };
    users.groups.timew-api = { };

    systemd.tmpfiles.rules = [ "d ${cfg.dataDir} 0750 timew-api timew-api -" ];

    systemd.services.timew-api = {
      description = "timewarrior HTTP API";
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      wantedBy = [ "multi-user.target" ];
      # timew turns bare times into dates in the local zone, and the weekly
      # report is in Irish time, so the service runs in it explicitly.
      environment.TZ = "Europe/Dublin";
      serviceConfig = {
        ExecStart = "${cfg.package}/bin/timew-api --host 127.0.0.1 --port ${toString cfg.port} --db ${cfg.dataDir}";
        EnvironmentFile = cfg.environmentFile;
        User = "timew-api";
        Group = "timew-api";
        Restart = "on-failure";
        RestartSec = 5;

        # It runs one binary and writes one directory.
        ReadWritePaths = [ cfg.dataDir ];
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        NoNewPrivileges = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectControlGroups = true;
        RestrictAddressFamilies = [ "AF_INET" "AF_INET6" "AF_UNIX" ];
        RestrictNamespaces = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        SystemCallArchitectures = "native";
        CapabilityBoundingSet = "";
        UMask = "0027";
      };
    };
  };
}

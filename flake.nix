{
  description = "timew-api: HTTP API and web page for timewarrior";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAll (pkgs: {
        default = pkgs.buildGoModule {
          pname = "timew-api";
          version = "0.1.0";
          src = self;
          vendorHash = null; # standard library only
          env.CGO_ENABLED = 0;
          ldflags = [ "-s" "-w" ];
          # The server shells out to `timew`; the end-to-end tests need it too.
          nativeBuildInputs = [ pkgs.makeWrapper ];
          nativeCheckInputs = [ pkgs.timewarrior ];
          # timew is always run in Europe/Dublin (internal/timew), and needs
          # the zone files to find it; the sandbox has none.
          preCheck = "export TZDIR=${pkgs.tzdata}/share/zoneinfo";
          postInstall = ''
            wrapProgram $out/bin/timew-api \
              --prefix PATH : ${pkgs.lib.makeBinPath [ pkgs.timewarrior ]} \
              --set-default TZDIR ${pkgs.tzdata}/share/zoneinfo
          '';
          meta.mainProgram = "timew-api";
        };
      });

      nixosModules.default = import ./nix/module.nix self;
    };
}

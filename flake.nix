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
        default = pkgs.python3Packages.buildPythonApplication {
          pname = "timew-api";
          version = "0.1.0";
          pyproject = true;
          src = self;
          build-system = [ pkgs.python3Packages.setuptools ];
          dependencies = [ pkgs.python3Packages.cryptography ];
          # The server shells out to `timew`; the end-to-end tests need it too.
          makeWrapperArgs = [ "--prefix" "PATH" ":" "${pkgs.lib.makeBinPath [ pkgs.timewarrior ]}" ];
          nativeCheckInputs = [ pkgs.timewarrior pkgs.tzdata ];
          checkPhase = ''
            runHook preCheck
            HOME=$TMPDIR TZ=Europe/Dublin python -m unittest discover -s tests -t .
            runHook postCheck
          '';
        };
      });

      nixosModules.default = import ./nix/module.nix self;
    };
}

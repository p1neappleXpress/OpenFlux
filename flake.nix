{
  description = "OpenFlux - Universal Bypass Tool";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
      pkgs = forAllSystems (system: nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (system: rec {
        openflux = pkgs.${system}.callPackage ./nix/package.nix {
          inherit self;
        };
        default = openflux;
      });

      devShells = forAllSystems (system: {
        default = pkgs.${system}.callPackage ./nix/shell.nix { };
      });
    };
}

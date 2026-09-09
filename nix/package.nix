{
  lib,
  buildGoModule,
  go_1_26,
  self,
}:
(buildGoModule.override { go = go_1_26; }) {
  pname = "openflux";
  version = "0.1.0";
  src = self;
  vendorHash = "sha256-MITicQOVM8O22ZS2iO5Lx9pIqtXNTQ2MBzan4Q1PjDI=";
  ldflags = [
    "-s"
    "-w"
  ];

  meta.mainProgram = "universal-bypass-tool";
}

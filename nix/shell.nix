{
  mkShell,
  go_1_26,
  gopls,
  delve,
  nixfmt,
}:
mkShell {
  packages = [
    go_1_26
    gopls
    delve
    nixfmt
  ];
}

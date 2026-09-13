// fluxcore is the dependency-free brain of Flux Core.
// Kept as its own module so it builds and unit-tests without the
// gVisor / pion transport stack (which needs Go 1.26.4).
// Integrated into OpenFlux via a thin adapter (see README).
module fluxcore

go 1.22

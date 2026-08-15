# boomerang

Measures end-to-end **and per-leg** RTT through a chain of relay nodes from a
single probe.

```
boomerang 192.0.2.101 198.51.100.20        # via one relay
boomerang 198.51.100.20                    # direct
boomerang --agent                          # on each relay and the destination
```

Work in progress. See `.hermes/plans/` for the implementation plan.

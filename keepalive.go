package recws

// defaultPingDivisor sets the default ping interval to a third of the
// keepalive timeout: two pings can be lost, or answered late, before the
// read deadline passes.
const defaultPingDivisor = 3

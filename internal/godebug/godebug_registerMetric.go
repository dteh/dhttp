package godebug

// func godebug_registerMetric(name string, read func() uint64) {
// 	metricsLock()
// 	initMetrics()
// 	d, ok := metrics[name]
// 	if !ok {
// 		throw("runtime: unexpected metric registration for " + name)
// 	}
// 	d.compute = metricReader(read).compute
// 	metrics[name] = d
// 	metricsUnlock()
// }

// func metricsLock() {
// 	// Acquire the metricsSema but with handoff. Operations are typically
// 	// expensive enough that queueing up goroutines and handing off between
// 	// them will be noticeably better-behaved.
// 	semacquire1(&metricsSema, true, 0, 0, waitReasonSemacquire)
// 	if raceenabled {
// 		raceacquire(unsafe.Pointer(&metricsSema))
// 	}
// }

// Package markets owns the market-metadata cache that every build_* tool
// path depends on for ticker→clobPairId resolution and unit-conversion
// constants (atomicResolution, stepBaseQuantums, subticksPerTick).
//
// Cache is populated at boot from chain (clob.Query/ClobPairAll) and the
// indexer (list_markets, which carries the human ticker strings), and is
// refreshed periodically (default 60s).
//
// ResolveTicker is the single entry point exposed to builders so the
// metadata lookup happens in exactly one place.
package markets

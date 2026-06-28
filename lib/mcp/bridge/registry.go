// Package bridge holds the cross-chain route registry that backs the
// build_bridge_deposit tool. It is pure data — the (sourceToken, targetChainId)
// -> targetToken whitelist the SVPBridge contract enforces on chain, mirrored
// off chain so the tool can resolve a human "bridge USDC to sepolia" request
// into the exact addresses the deposit call needs (and reject pairs the bridge
// would never honor, before building a tx that reverts).
//
// The registry is loaded from an operator-supplied JSON file (evm_bridge_routes_path).
// The file is an array of route objects in the shape the bridge backend already
// publishes:
//
//	[{"srcChain":"svp_chain","srcChainId":2517,"targetChain":"sepolia",
//	  "targetChainId":11155111,"srcToken":"0x..","targetToken":"0x..",
//	  "symbol":"USDC","decimals":6}, ...]
//
// srcToken / targetToken of the zero address (0x000…000) denote the native coin
// on that chain. The file may contain routes in every direction; the tool scopes
// lookups to a configured source chain id, so inbound routes are simply ignored.
package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// Route is one resolved cross-chain pair: bridging SrcToken on SrcChainID to
// TargetChainID releases TargetToken there. Decimals is the source asset's
// decimals (used to convert human amounts at the deposit boundary).
type Route struct {
	SrcChainID    uint64
	TargetChainID uint64
	TargetChain   string
	SrcToken      common.Address
	TargetToken   common.Address
	Symbol        string
	Decimals      int64
}

// NativeSource reports whether the source asset is the native coin (the bridge's
// depositNative entrypoint, value-carried) rather than an ERC-20.
func (r Route) NativeSource() bool { return r.SrcToken == (common.Address{}) }

// ChainRef is a (name, id) pair surfaced for discovery / error messages.
type ChainRef struct {
	Name string
	ID   uint64
}

// Registry is an immutable, concurrent-safe view of the loaded routes.
type Registry struct {
	routes []Route
	// chainID -> canonical chain name, and lower(name) -> chainID, gathered
	// from both columns so any known chain resolves by name or numeric id.
	idToName map[uint64]string
	nameToID map[string]uint64
}

// rawRoute mirrors one element of the routes JSON file.
type rawRoute struct {
	SrcChain      string `json:"srcChain"`
	SrcChainID    uint64 `json:"srcChainId"`
	TargetChain   string `json:"targetChain"`
	TargetChainID uint64 `json:"targetChainId"`
	SrcToken      string `json:"srcToken"`
	TargetToken   string `json:"targetToken"`
	Symbol        string `json:"symbol"`
	Decimals      int64  `json:"decimals"`
}

// LoadRegistry reads and validates the routes file at path.
func LoadRegistry(path string) (*Registry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read bridge routes %s: %w", path, err)
	}
	var raws []rawRoute
	if err := json.Unmarshal(b, &raws); err != nil {
		return nil, fmt.Errorf("parse bridge routes %s: %w", path, err)
	}
	if len(raws) == 0 {
		return nil, fmt.Errorf("bridge routes %s is empty", path)
	}
	return newRegistry(raws)
}

func newRegistry(raws []rawRoute) (*Registry, error) {
	reg := &Registry{
		idToName: map[uint64]string{},
		nameToID: map[string]uint64{},
	}
	// Detect duplicate (src, target, srcToken) and (src, target, symbol) keys —
	// either would make a lookup ambiguous.
	seenToken := map[string]bool{}
	seenSymbol := map[string]bool{}
	for i, r := range raws {
		if r.SrcChainID == 0 || r.TargetChainID == 0 {
			return nil, fmt.Errorf("route[%d] (%s->%s) has a zero chain id", i, r.SrcChain, r.TargetChain)
		}
		if !common.IsHexAddress(r.SrcToken) {
			return nil, fmt.Errorf("route[%d] srcToken %q is not a 0x address", i, r.SrcToken)
		}
		if !common.IsHexAddress(r.TargetToken) {
			return nil, fmt.Errorf("route[%d] targetToken %q is not a 0x address", i, r.TargetToken)
		}
		if strings.TrimSpace(r.Symbol) == "" {
			return nil, fmt.Errorf("route[%d] (%s->%s) has an empty symbol", i, r.SrcChain, r.TargetChain)
		}
		if r.Decimals < 0 || r.Decimals > 77 {
			return nil, fmt.Errorf("route[%d] (%s) decimals %d out of range", i, r.Symbol, r.Decimals)
		}
		route := Route{
			SrcChainID:    r.SrcChainID,
			TargetChainID: r.TargetChainID,
			TargetChain:   r.TargetChain,
			SrcToken:      common.HexToAddress(r.SrcToken),
			TargetToken:   common.HexToAddress(r.TargetToken),
			Symbol:        r.Symbol,
			Decimals:      r.Decimals,
		}
		tokenKey := routeKey(route.SrcChainID, route.TargetChainID, route.SrcToken.Hex())
		if seenToken[tokenKey] {
			return nil, fmt.Errorf("duplicate route for src %d -> target %d token %s",
				route.SrcChainID, route.TargetChainID, route.SrcToken.Hex())
		}
		seenToken[tokenKey] = true
		symKey := routeKey(route.SrcChainID, route.TargetChainID, strings.ToLower(route.Symbol))
		if seenSymbol[symKey] {
			return nil, fmt.Errorf("duplicate route for src %d -> target %d symbol %s",
				route.SrcChainID, route.TargetChainID, route.Symbol)
		}
		seenSymbol[symKey] = true

		reg.routes = append(reg.routes, route)
		reg.learnChain(r.SrcChain, r.SrcChainID)
		reg.learnChain(r.TargetChain, r.TargetChainID)
	}
	return reg, nil
}

func (r *Registry) learnChain(name string, id uint64) {
	if name == "" {
		return
	}
	if _, ok := r.idToName[id]; !ok {
		r.idToName[id] = name
	}
	r.nameToID[strings.ToLower(name)] = id
}

func routeKey(src, target uint64, last string) string {
	return fmt.Sprintf("%d|%d|%s", src, target, strings.ToLower(last))
}

// ResolveDestChain resolves a destination argument (a chain name like "sepolia"
// / "arbitrum_sepolia", or a numeric chain id like "11155111") to its chain id,
// validating that at least one route exists from srcChainID to it. Used by the
// outbound build_bridge_deposit tool, where the source (svpchain) is fixed.
func (r *Registry) ResolveDestChain(query string, srcChainID uint64) (uint64, error) {
	return r.resolveChain(query, srcChainID, false)
}

// ResolveSourceChain resolves a source argument (a foreign chain name like
// "sepolia" / "arbitrum_sepolia", or a numeric chain id) to its chain id,
// validating that at least one route exists from it into targetChainID. The
// inbound twin of ResolveDestChain: used by build_bridge_deposit_inbound, where
// the target (svpchain) is fixed and the foreign source is what we resolve.
func (r *Registry) ResolveSourceChain(query string, targetChainID uint64) (uint64, error) {
	return r.resolveChain(query, targetChainID, true)
}

// resolveChain is the shared body of ResolveDestChain / ResolveSourceChain.
// fixedID is the chain held constant (svpchain); fixedIsTarget selects the
// direction: when true we resolve a source chain bridging INTO fixedID
// (inbound), when false a destination chain bridging out OF fixedID (outbound).
func (r *Registry) resolveChain(query string, fixedID uint64, fixedIsTarget bool) (uint64, error) {
	noun, hint := "destination", r.targetHint(fixedID)
	if fixedIsTarget {
		noun, hint = "source", r.sourceHint(fixedID)
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return 0, fmt.Errorf("%s chain is required (e.g. %s)", noun, hint)
	}
	var id uint64
	if n, err := strconv.ParseUint(q, 10, 64); err == nil {
		id = n
	} else if cid, ok := r.nameToID[strings.ToLower(q)]; ok {
		id = cid
	} else {
		return 0, fmt.Errorf("unknown %s chain %q; available: %s", noun, query, hint)
	}
	if id == fixedID {
		return 0, fmt.Errorf("%s chain %q is the same as the %s chain — nothing to bridge",
			noun, query, map[bool]string{true: "target", false: "source"}[fixedIsTarget])
	}
	// Direction-aware route existence check: inbound looks for id -> fixedID,
	// outbound for fixedID -> id.
	src, target := fixedID, id
	if fixedIsTarget {
		src, target = id, fixedID
	}
	if len(r.routesFor(src, target)) == 0 {
		return 0, fmt.Errorf("no bridge route %s chain %q; available %ss: %s",
			map[bool]string{true: "from", false: "to"}[fixedIsTarget], query, noun, hint)
	}
	return id, nil
}

// Lookup resolves a token argument for a (srcChainID -> targetChainID) leg.
// tokenQuery may be a symbol ("USDC", case-insensitive), a 0x source-token
// address, or empty/"native" for the native coin.
func (r *Registry) Lookup(srcChainID, targetChainID uint64, tokenQuery string) (Route, error) {
	q := strings.TrimSpace(tokenQuery)
	routes := r.routesFor(srcChainID, targetChainID)
	if len(routes) == 0 {
		return Route{}, fmt.Errorf("no bridge routes from chain %d to chain %d", srcChainID, targetChainID)
	}

	switch strings.ToLower(q) {
	case "", "native":
		for _, rt := range routes {
			if rt.NativeSource() {
				return rt, nil
			}
		}
		return Route{}, fmt.Errorf("native coin is not bridgeable to chain %d; bridgeable tokens: %s",
			targetChainID, tokenHint(routes))
	}

	if common.IsHexAddress(q) {
		addr := common.HexToAddress(q)
		for _, rt := range routes {
			if rt.SrcToken == addr {
				return rt, nil
			}
		}
		return Route{}, fmt.Errorf("token %s is not bridgeable to chain %d; bridgeable tokens: %s",
			addr.Hex(), targetChainID, tokenHint(routes))
	}

	for _, rt := range routes {
		if strings.EqualFold(rt.Symbol, q) {
			return rt, nil
		}
	}
	return Route{}, fmt.Errorf("token %q is not bridgeable to chain %d; bridgeable tokens: %s",
		tokenQuery, targetChainID, tokenHint(routes))
}

// routesFor returns the routes from src to target, in registry order.
func (r *Registry) routesFor(src, target uint64) []Route {
	var out []Route
	for _, rt := range r.routes {
		if rt.SrcChainID == src && rt.TargetChainID == target {
			out = append(out, rt)
		}
	}
	return out
}

// AvailableTargets lists the distinct destination chains reachable from
// srcChainID, sorted by chain id.
func (r *Registry) AvailableTargets(srcChainID uint64) []ChainRef {
	seen := map[uint64]bool{}
	var out []ChainRef
	for _, rt := range r.routes {
		if rt.SrcChainID != srcChainID || seen[rt.TargetChainID] {
			continue
		}
		seen[rt.TargetChainID] = true
		out = append(out, ChainRef{Name: r.idToName[rt.TargetChainID], ID: rt.TargetChainID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// AvailableSources lists the distinct foreign chains that can bridge INTO
// targetChainID, sorted by chain id. The inbound twin of AvailableTargets.
func (r *Registry) AvailableSources(targetChainID uint64) []ChainRef {
	seen := map[uint64]bool{}
	var out []ChainRef
	for _, rt := range r.routes {
		if rt.TargetChainID != targetChainID || seen[rt.SrcChainID] {
			continue
		}
		seen[rt.SrcChainID] = true
		out = append(out, ChainRef{Name: r.idToName[rt.SrcChainID], ID: rt.SrcChainID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// HasSource reports whether the registry contains any route originating from
// srcChainID — used at startup to fail loudly on a source-chain misconfiguration.
func (r *Registry) HasSource(srcChainID uint64) bool {
	for _, rt := range r.routes {
		if rt.SrcChainID == srcChainID {
			return true
		}
	}
	return false
}

// HasTarget reports whether the registry contains any route terminating at
// targetChainID — used at startup to fail loudly when an inbound source chain
// is configured but has no route into svpchain. The inbound twin of HasSource.
func (r *Registry) HasTarget(targetChainID uint64) bool {
	for _, rt := range r.routes {
		if rt.TargetChainID == targetChainID {
			return true
		}
	}
	return false
}

func (r *Registry) targetHint(srcChainID uint64) string {
	return chainHint(r.AvailableTargets(srcChainID))
}

func (r *Registry) sourceHint(targetChainID uint64) string {
	return chainHint(r.AvailableSources(targetChainID))
}

func chainHint(chains []ChainRef) string {
	if len(chains) == 0 {
		return "(none configured)"
	}
	parts := make([]string, len(chains))
	for i, c := range chains {
		parts[i] = fmt.Sprintf("%s (%d)", c.Name, c.ID)
	}
	return strings.Join(parts, ", ")
}

func tokenHint(routes []Route) string {
	parts := make([]string, len(routes))
	for i, rt := range routes {
		parts[i] = rt.Symbol
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

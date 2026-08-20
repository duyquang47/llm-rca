// Package drain implements the Drain log-clustering algorithm: a
// fixed-depth parse tree that groups near-identical log lines into
// templates, replacing variable tokens with <*>. See CLAUDE.md §6.3.
//
// There's no mature Go port of drain3, so this is a from-scratch
// implementation of the same core idea:
//  1. Group by token count (layer 1).
//  2. Branch on token value at position 1, 2, ... (layers 2..maxDepth),
//     treating any token containing a digit as a wildcard branch key
//     so numeric variants don't fragment the tree.
//  3. At the leaf, compare against existing clusters by positional
//     token similarity; merge into the best match above simThreshold,
//     updating the template (differing positions -> <*>), or start a
//     new cluster.
package drain

import (
	"strconv"
	"strings"
	"time"
)

const (
	defaultMaxDepth     = 4
	defaultSimThreshold = 0.4
	wildcard            = "<*>"
	numericBranchKey    = "<NUM>"
)

type Template struct {
	ID        int
	Template  string
	Count     int
	FirstSeen time.Time
	LastSeen  time.Time
}

type Drain interface {
	// Add parses msg into the tree and returns the (possibly new)
	// template it was assigned to.
	Add(msg string) (templateID int, template string)
	Templates() []Template
}

type logCluster struct {
	id        int
	tokens    []string
	count     int
	firstSeen time.Time
	lastSeen  time.Time
}

type node struct {
	children map[string]*node
	clusters []*logCluster
}

func newNode() *node {
	return &node{children: make(map[string]*node)}
}

type tree struct {
	root         *node
	maxDepth     int
	simThreshold float64
	nextID       int
	now          func() time.Time
}

// New returns a Drain with the defaults CLAUDE.md §6.3 specifies
// (simThreshold 0.4).
func New() Drain {
	return &tree{
		root:         newNode(),
		maxDepth:     defaultMaxDepth,
		simThreshold: defaultSimThreshold,
		now:          time.Now,
	}
}

func (t *tree) Add(msg string) (int, string) {
	tokens := tokenize(msg)
	leaf := t.walk(tokens)
	cluster := t.bestMatch(leaf, tokens)

	now := t.now()
	if cluster == nil {
		t.nextID++
		cluster = &logCluster{
			id:        t.nextID,
			tokens:    append([]string(nil), tokens...),
			firstSeen: now,
		}
		leaf.clusters = append(leaf.clusters, cluster)
	} else {
		mergeTemplate(cluster.tokens, tokens)
	}
	cluster.count++
	cluster.lastSeen = now

	return cluster.id, strings.Join(cluster.tokens, " ")
}

func (t *tree) Templates() []Template {
	var out []Template
	var walk func(n *node)
	walk = func(n *node) {
		for _, c := range n.clusters {
			out = append(out, Template{
				ID:        c.id,
				Template:  strings.Join(c.tokens, " "),
				Count:     c.count,
				FirstSeen: c.firstSeen,
				LastSeen:  c.lastSeen,
			})
		}
		for _, child := range n.children {
			walk(child)
		}
	}
	walk(t.root)
	return out
}

// walk descends the tree by token count, then by branch key at each
// position up to maxDepth, creating nodes as needed, and returns the
// leaf node log lines with this token shape should be compared at.
func (t *tree) walk(tokens []string) *node {
	n := t.child(t.root, strconv.Itoa(len(tokens)))

	depth := t.maxDepth - 1 // one level already spent on token count
	for i := 0; i < len(tokens) && i < depth; i++ {
		n = t.child(n, branchKey(tokens[i]))
	}
	return n
}

func (t *tree) child(n *node, key string) *node {
	if c, ok := n.children[key]; ok {
		return c
	}
	c := newNode()
	n.children[key] = c
	return c
}

func branchKey(token string) string {
	if hasDigit(token) {
		return numericBranchKey
	}
	return token
}

// bestMatch finds the cluster at leaf whose template is most similar
// to tokens (matching positions / total), above simThreshold. All
// clusters at a leaf share the same token count by construction.
func (t *tree) bestMatch(leaf *node, tokens []string) *logCluster {
	var best *logCluster
	bestSim := t.simThreshold
	for _, c := range leaf.clusters {
		sim := similarity(c.tokens, tokens)
		if sim >= bestSim {
			bestSim = sim
			best = c
		}
	}
	return best
}

func similarity(template, tokens []string) float64 {
	if len(template) == 0 {
		return 0
	}
	matches := 0
	for i, tok := range template {
		if tok == wildcard || tok == tokens[i] {
			matches++
		}
	}
	return float64(matches) / float64(len(template))
}

// mergeTemplate updates template in place: any position that
// disagrees with tokens (and isn't already a wildcard) becomes one.
func mergeTemplate(template, tokens []string) {
	for i, tok := range template {
		if tok != wildcard && tok != tokens[i] {
			template[i] = wildcard
		}
	}
}

func tokenize(msg string) []string {
	return strings.Fields(msg)
}

func hasDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

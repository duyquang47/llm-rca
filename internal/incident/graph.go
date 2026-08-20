package incident

import "github.com/duyquang47/llm-rca/internal/contextpack"

// withinHops reports whether b is reachable from a within maxHops
// edges of graph, treating edges as undirected — alert correlation
// doesn't care which direction a call went, only that the two
// services are close in the dependency chain.
func withinHops(graph contextpack.Graph, a, b string, maxHops int) bool {
	if a == b {
		return true
	}
	adjacency := make(map[string][]string)
	for _, e := range graph.Edges {
		adjacency[e.From] = append(adjacency[e.From], e.To)
		adjacency[e.To] = append(adjacency[e.To], e.From)
	}

	visited := map[string]bool{a: true}
	frontier := []string{a}
	for hop := 0; hop < maxHops && len(frontier) > 0; hop++ {
		var next []string
		for _, node := range frontier {
			for _, neighbor := range adjacency[node] {
				if neighbor == b {
					return true
				}
				if !visited[neighbor] {
					visited[neighbor] = true
					next = append(next, neighbor)
				}
			}
		}
		frontier = next
	}
	return false
}

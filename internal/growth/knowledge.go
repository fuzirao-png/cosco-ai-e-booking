package growth

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
)

//go:embed handbook.txt
var handbook string

type Citation struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Ports          []string `json:"ports"`
	ContainerTypes []string `json:"container_types"`
}

type Chunk struct {
	Citation
	Text string
}

var (
	headingPattern   = regexp.MustCompile(`(?m)^## `)
	containerPattern = regexp.MustCompile(`\b(20GP|40GP|40HC)\b`)
	wordPattern      = regexp.MustCompile(`[a-z0-9]+|[\x{4e00}-\x{9fff}]+`)
)

func chunkDocument(text string) ([]Chunk, error) {
	var chunks []Chunk
	seen := map[string]bool{}
	for _, section := range headingPattern.Split(text, -1) {
		section = strings.TrimSpace(section)
		if section == "" {
			continue
		}
		titleLine, body, found := strings.Cut(section, "\n")
		id, title, split := strings.Cut(titleLine, " | ")
		if !found || !split || seen[id] {
			return nil, fmt.Errorf("invalid knowledge heading %q", titleLine)
		}
		seen[id] = true
		metadata := strings.ToUpper(section)
		source := Citation{ID: id, Title: title, Ports: []string{}, ContainerTypes: []string{}}
		for _, port := range ports {
			if strings.Contains(metadata, port.code) {
				source.Ports = append(source.Ports, port.code)
			}
		}
		for _, kind := range containerPattern.FindAllString(metadata, -1) {
			if !slices.Contains(source.ContainerTypes, kind) {
				source.ContainerTypes = append(source.ContainerTypes, kind)
			}
		}
		chunks = append(chunks, Chunk{source, strings.TrimSpace(body)})
	}
	return chunks, nil
}

func tokens(text string) []string {
	var result []string
	for _, word := range wordPattern.FindAllString(strings.ToLower(text), -1) {
		if word[0] < 128 {
			result = append(result, word)
			continue
		}
		runes := []rune(word)
		for _, r := range runes {
			result = append(result, string(r))
		}
		for i := 0; i+1 < len(runes); i++ {
			result = append(result, string(runes[i:i+2]))
		}
	}
	return result
}

func frequencies(tokens []string) map[string]int {
	counts := make(map[string]int)
	for _, term := range tokens {
		counts[term]++
	}
	return counts
}

func normalize(vector []float64) {
	var norm float64
	for _, value := range vector {
		norm += value * value
	}
	if norm == 0 {
		return
	}
	norm = math.Sqrt(norm)
	for i := range vector {
		vector[i] /= norm
	}
}

func localVector(text string) []float64 {
	vector := make([]float64, 512)
	terms := tokens(text)
	counts := frequencies(terms)
	for _, term := range terms {
		count, exists := counts[term]
		if !exists {
			continue
		}
		delete(counts, term)
		digest := sha256.Sum256([]byte(term))
		bucket := binary.LittleEndian.Uint32(digest[:4])
		weight := 1 + math.Log(float64(count))
		if bucket&512 == 0 {
			weight = -weight
		}
		vector[bucket%512] += weight
	}
	normalize(vector)
	return vector
}

type Embedder interface {
	Embed(context.Context, []string) ([][]float64, error)
}

type Index struct {
	chunks                   []Chunk
	counts                   []map[string]int
	lengths                  []int
	averageLength            float64
	documentFrequency        map[string]int
	localVectors, bgeVectors [][]float64
	embedder                 Embedder
}

func NewIndex(ctx context.Context, embedder Embedder) (*Index, error) {
	var source strings.Builder
	source.WriteString(handbook)
	for _, rate := range tariffs {
		fmt.Fprintf(&source, "\n## %s | %s → %s %s\n%s\n", rate.id(),
			portName(rate.origin), portName(rate.destination), rate.kind, rate.body())
	}
	chunks, err := chunkDocument(source.String())
	if err != nil {
		return nil, err
	}
	index := &Index{chunks: chunks, documentFrequency: map[string]int{}, embedder: embedder}
	texts := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		text := chunk.Title + "\n" + chunk.Text
		texts = append(texts, text)
		terms := tokens(text)
		index.averageLength += float64(len(terms))
		index.lengths = append(index.lengths, len(terms))
		counts := frequencies(terms)
		for term := range counts {
			index.documentFrequency[term]++
		}
		index.counts = append(index.counts, counts)
		index.localVectors = append(index.localVectors, localVector(text))
	}
	index.averageLength /= float64(len(chunks))
	if embedder != nil {
		index.bgeVectors, err = embedder.Embed(ctx, texts)
		if err != nil {
			return nil, fmt.Errorf("index BGE documents: %w", err)
		}
	}
	return index, nil
}

func (k *Index) Search(ctx context.Context, query string) ([]Chunk, error) {
	terms := tokens(query)
	slices.Sort(terms)
	terms = slices.Compact(terms)
	vectors := k.localVectors
	vector := localVector(query)
	var embedErr error
	if k.embedder != nil {
		encoded, err := k.embedder.Embed(ctx, []string{"为这个句子生成表示以用于检索相关文章：" + query})
		if err != nil {
			embedErr = err
		} else if len(encoded) != 1 || len(encoded[0]) != len(k.bgeVectors[0]) {
			embedErr = fmt.Errorf("embedding dimension changed")
		} else {
			vector, vectors = encoded[0], k.bgeVectors
		}
	}

	var scores [][2]float64
	for i, counts := range k.counts {
		scale := 1.2 * (0.25 + 0.75*float64(k.lengths[i])/k.averageLength)
		var bm25, similarity float64
		for _, term := range terms {
			count := float64(counts[term])
			if count == 0 {
				continue
			}
			df := float64(k.documentFrequency[term])
			bm25 += math.Log(1+(float64(len(k.chunks))-df+0.5)/(df+0.5)) * count * 2.2 / (count + scale)
		}
		for j, value := range vector {
			similarity += value * vectors[i][j]
		}
		scores = append(scores, [2]float64{bm25, similarity})
	}
	fused := make([]float64, len(scores))
	ranked := make([]int, len(scores))
	for channel := range 2 {
		for i := range ranked {
			ranked[i] = i
		}
		sort.SliceStable(ranked, func(i, j int) bool { return scores[ranked[i]][channel] > scores[ranked[j]][channel] })
		for rank, i := range ranked[:min(20, len(ranked))] {
			if scores[i][channel] > 0 {
				fused[i] += 1 / float64(61+rank)
			}
		}
	}
	for i := range ranked {
		ranked[i] = i
	}
	sort.SliceStable(ranked, func(i, j int) bool { return fused[ranked[i]] > fused[ranked[j]] })
	chunks := []Chunk{}
	for _, i := range ranked[:min(8, len(ranked))] {
		if fused[i] <= 0 {
			break
		}
		chunks = append(chunks, k.chunks[i])
	}
	return chunks, embedErr
}

func (k *Index) citation(id string) []Chunk {
	for _, chunk := range k.chunks {
		if chunk.ID == id {
			return []Chunk{chunk}
		}
	}
	return nil
}

import hashlib
import math
import os
import re
from collections import Counter
from dataclasses import dataclass
from functools import lru_cache
from pathlib import Path

from .catalog import PORTS, TARIFFS


@dataclass(frozen=True)
class Chunk:
    id: str
    title: str
    raw_text: str
    ports: tuple[str, ...]
    container_types: tuple[str, ...]

    def citation(self) -> dict:
        return {"id": self.id, "title": self.title, "ports": self.ports,
                "container_types": self.container_types}


def chunk_document(raw_text: str) -> list[Chunk]:
    chunks = []
    for section in re.split(r"(?m)^## ", raw_text):
        if not section.strip():
            continue
        heading, body = section.strip().split("\n", 1)
        key, title = heading.split(" | ", 1)
        metadata = " ".join((heading, body)).upper()
        chunks.append(Chunk(
            key, title, body.strip(),
            tuple(port for port in PORTS if port in metadata),
            tuple(dict.fromkeys(re.findall(r"\b(?:20GP|40GP|40HC)\b", metadata, re.ASCII))),
        ))
    return chunks


def tokens(value: str) -> list[str]:
    words = re.findall(r"[a-z0-9]+|[\u4e00-\u9fff]+", value.lower())
    return [
        token for word in words
        for token in ([word] if word.isascii() else
                      [*word, *(word[i:i + 2] for i in range(len(word) - 1))])
    ]


def local_vector(value: str) -> list[float]:
    features = Counter(tokens(value))
    vector = [0.0] * 512
    for feature, frequency in features.items():
        bucket = int.from_bytes(hashlib.blake2s(feature.encode(), digest_size=4).digest(), "little")
        vector[bucket % len(vector)] += (1 if bucket & 512 else -1) * (1 + math.log(frequency))
    norm = math.sqrt(sum(component * component for component in vector)) or 1
    return [component / norm for component in vector]


class KnowledgeBase:
    def __init__(self, mode: str):
        handbook = Path(__file__).with_name("handbook.txt").read_text(encoding="utf-8")
        tariffs = "\n".join(f"## {rate.id} | {PORTS[rate.origin]} → {PORTS[rate.destination]} "
                            f"{rate.container_type}\n{rate.text}" for rate in TARIFFS)
        self.documents = chunk_document(f"{handbook}\n{tariffs}")
        self.counters = [Counter(tokens(f"{chunk.title} {chunk.raw_text}"))
                         for chunk in self.documents]
        self.lengths = [sum(counter.values()) for counter in self.counters]
        self.avg_length = sum(self.lengths) / len(self.lengths)
        self.df = Counter(term for counter in self.counters for term in counter)
        self.embedder = None
        if mode == "bge":
            from fastembed import TextEmbedding
            self.embedder = TextEmbedding("BAAI/bge-small-zh-v1.5", cache_dir=".bge-cache")
        elif mode != "local":
            raise ValueError("GROWTH_EMBEDDINGS must be local or bge")
        values = [f"{chunk.title}\n{chunk.raw_text}" for chunk in self.documents]
        self.vectors = list(self.embedder.passage_embed(values)) if self.embedder else [
            local_vector(value) for value in values
        ]

    def search(self, query: str, limit: int = 8) -> list[Chunk]:
        lexical = Counter(tokens(query))
        query_vector = (next(iter(self.embedder.query_embed(query))) if self.embedder
                        else local_vector(query))
        scores = []
        for index, counter in enumerate(self.counters):
            scale = 1.2 * (0.25 + 0.75 * self.lengths[index] / self.avg_length)
            bm25 = sum(
                math.log(1 + (len(self.documents) - self.df[term] + 0.5) / (self.df[term] + 0.5))
                * (counter[term] * 2.2) / (counter[term] + scale)
                for term in lexical if term in counter
            )
            similarity = sum(float(a) * float(b) for a, b in zip(query_vector, self.vectors[index]))
            scores.append((bm25, similarity))
        fused: Counter[int] = Counter()
        for column in (0, 1):
            ranked = sorted(range(len(scores)), key=lambda index: -scores[index][column])
            for rank, index in enumerate(ranked[:20], 1):
                if scores[index][column] > 0:
                    fused[index] += 1 / (60 + rank)
        return [self.documents[index] for index, _ in fused.most_common(limit)]


@lru_cache(maxsize=2)
def default_index(mode: str | None = None) -> KnowledgeBase:
    return KnowledgeBase(mode or os.getenv("GROWTH_EMBEDDINGS", "local"))

"""Retrieval adapters for the DevRouter benchmark harness.

Each adapter wraps a memory/context system and exposes a uniform
`query(q, repo, k) -> AdapterResult` interface so the harness can score
them apples-to-apples on the same question set.
"""

from .base import Adapter, AdapterResult, REGISTRY, register

__all__ = ["Adapter", "AdapterResult", "REGISTRY", "register"]

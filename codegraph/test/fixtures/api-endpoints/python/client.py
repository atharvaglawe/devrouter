"""Client-side fixtures for the generic Python API-endpoint extractor."""

import requests
import httpx
import aiohttp


def use_requests():
    requests.get("/api/items")
    requests.post("/api/items", json={})
    requests.delete("/api/items/1")
    requests.request("PUT", "/api/items/1")


def use_requests_session():
    s = requests.Session()
    s.get("/api/users")
    s.post("/api/users", json={})


def use_httpx():
    httpx.get("/api/health")
    httpx.AsyncClient().delete("/api/x")


async def use_httpx_async():
    async with httpx.AsyncClient() as client:
        await client.get("/api/async")


async def use_aiohttp():
    session = aiohttp.ClientSession()
    await session.get("/api/aio")
    await session.post("/api/aio", json={})

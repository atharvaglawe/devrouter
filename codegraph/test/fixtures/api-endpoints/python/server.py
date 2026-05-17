"""Server-side fixtures for the generic Python API-endpoint extractor."""

from fastapi import FastAPI, APIRouter
from flask import Flask, Blueprint
from django.urls import path, re_path
from aiohttp import web
import tornado.web

# FastAPI app + router with prefix.
app = FastAPI()
router = APIRouter(prefix="/api/v1")


@app.get("/items")
async def list_items():
    return []


@app.post("/items")
def create_item(payload):
    return payload


@router.get("/users/{id}")
def get_user(id):
    return {}


@router.delete("/users/{id}")
def delete_user(id):
    return None


# Mount the router with an additional prefix at include time.
app.include_router(router, prefix="/v2-extra")


# Flask app + Blueprint with url_prefix.
flask_app = Flask(__name__)
bp = Blueprint("bp", __name__, url_prefix="/v2")


@flask_app.route("/legacy", methods=["POST", "GET"])
def legacy():
    return ""


@flask_app.route("/default-method")
def default_get():
    return ""


@bp.route("/things")
def things():
    return ""


# Handler stubs used by the registrations below.
def upload_handler(req):
    return req


def patch_handler(req):
    return req


class XHandler:
    pass


class YHandler:
    pass


def hello_view(req):
    return req


def book_detail(req, id):
    return id


def legacy_view(req):
    return req


# aiohttp web routes — both forms.
aio_app = web.Application()
aio_app.router.add_get("/health", lambda r: r)
aio_app.router.add_post("/upload", upload_handler)
aio_app.router.add_route("PATCH", "/patch-me", patch_handler)


# Tornado: list-of-tuples form.
tornado_app = tornado.web.Application(
    [
        (r"/tornado/x", XHandler),
        (r"/tornado/y/{id}", YHandler),
    ]
)


# Django urls.py-style.
urlpatterns = [
    path("hello/", hello_view),
    path("books/<int:id>/", book_detail),
    re_path(r"^legacy/?$", legacy_view),
]

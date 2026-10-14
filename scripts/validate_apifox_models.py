"""Validate the model-only OpenAPI artifact and its privacy boundaries."""

import json
from pathlib import Path

from jsonschema import Draft4Validator, FormatChecker


ROOT = Path(__file__).resolve().parents[1]
SPEC = json.loads((ROOT / "docs/api/models.openapi.json").read_text(encoding="utf-8"))
SCHEMAS = SPEC["components"]["schemas"]


def walk(value):
    if isinstance(value, dict):
        if "$ref" in value:
            pointer = value["$ref"]
            assert pointer.startswith("#/components/schemas/"), pointer
            assert pointer.rsplit("/", 1)[1] in SCHEMAS, pointer
        if value.get("type") == "object":
            assert set(value.get("required", [])) <= set(value.get("properties", {}))
        for nested in value.values():
            walk(nested)
    elif isinstance(value, list):
        for nested in value:
            walk(nested)


def validator(name):
    # Use the actual local component references under one Draft 4 schema root.
    return Draft4Validator(
        {"$ref": f"#/components/schemas/{name}", "components": SPEC["components"]},
        format_checker=FormatChecker(),
    )


def accepts(name, value):
    validator(name).validate(value)


def rejects(name, value):
    assert not validator(name).is_valid(value), (name, value)


def reachable(name, seen=None):
    seen = set() if seen is None else seen
    if name in seen:
        return seen
    seen.add(name)

    def scan(value):
        if isinstance(value, dict):
            if "$ref" in value:
                reachable(value["$ref"].rsplit("/", 1)[1], seen)
            for nested in value.values():
                scan(nested)
        elif isinstance(value, list):
            for nested in value:
                scan(nested)

    scan(SCHEMAS[name])
    return seen


assert SPEC["openapi"] == "3.0.3"
assert SPEC["paths"] == {}
assert len(SCHEMAS) == 50
walk(SPEC)
for schema in SCHEMAS.values():
    Draft4Validator.check_schema(schema)
assert not any(name.startswith("Admin") for name in reachable("PublicContent"))

anonymous = {"mode": "anonymous", "display_name": "楼主", "is_topic_owner": True}
accepts("ContentAuthor", anonymous)
for field in ("user", "user_id", "author_id", "public_id", "email", "avatar_url"):
    rejects("ContentAuthor", {**anonymous, field: "private-value"})

stamp = "2026-10-14T08:00:00Z"
comment = {
    "id": "101", "state": "published", "kind": "comment", "topic_id": "100",
    "author": anonymous, "body_md": "补充说明", "attachments": [], "created_at": stamp,
}
accepts("PublicContent", comment)
rejects("PublicContent", {**comment, "author_id": "7"})
tombstone = {
    "id": "101", "state": "deleted", "kind": "comment",
    "message": "该评论已被删除", "deleted_at": stamp, "topic_id": "100",
}
accepts("PublicContent", tombstone)
for field in ("body_md", "original_body_md", "title", "author", "attachments"):
    rejects("PublicContent", {**tombstone, field: "deleted-private-value"})

request = {"topic_id": "100", "body_md": "回复", "is_anonymous": True}
accepts("CreateCommentRequest", request)
rejects("CreateCommentRequest", {**request, "author_id": "7"})
accepts("SetResolutionRequest", {"resolution_status": "solved"})
accepts("SetResolutionRequest", {"resolution_status": "open"})
rejects("SetResolutionRequest", {"resolution_status": "accepted"})
rejects("EditContentRequest", {"body_md": "修改", "is_anonymous": False})
rejects("UpdateProfileRequest", {})
print(f"PASS: {len(SCHEMAS)} schemas, local references, schema shapes, and privacy fixtures; 0 paths.")

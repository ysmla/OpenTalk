"""Formal OpenAPI validation and structural MySQL migration checks."""
import json
import re
from pathlib import Path

import sqlglot
from openapi_spec_validator import validate

ROOT=Path(__file__).resolve().parents[1]
spec=json.loads((ROOT/'docs/api/openapi.json').read_text(encoding='utf-8'))
validate(spec)
validate(json.loads((ROOT/'docs/api/models.openapi.json').read_text(encoding='utf-8')))
operation_ids=set()
implemented=set()
for path, methods in spec['paths'].items():
    for method, operation in methods.items():
        oid=operation['operationId']
        assert oid not in operation_ids, oid
        operation_ids.add(oid)
        params={p['name'] for p in operation.get('parameters',[]) if p['in']=='path' and p['required']}
        assert params == set(re.findall(r'{(\w+)}',path)), path
        if path.startswith('/api/v1/admin/'):
            assert operation['x-required-role']=='admin'
            assert 'SessionCookie' in operation['security'][0]
        if method not in ('get','head') and operation['security']:
            assert 'CSRFToken' in operation['security'][0], path
        if operation['x-implementation-status']=='implemented': implemented.add(path)
# An operation may only be marked implemented if its path is listed here, so flipping the status
# in the contract cannot silently claim a capability that nothing in this repository verifies.
ALLOWED_IMPLEMENTED={
    '/api/v1/auth/captcha','/api/v1/auth/captcha/{challenge_id}/image','/api/v1/auth/email-codes',
    '/api/v1/auth/register','/api/v1/auth/login','/api/v1/auth/logout','/api/v1/auth/reset-password',
    '/api/v1/auth/csrf','/api/v1/me','/api/v1/admin/settings','/api/v1/boards','/api/v1/topics',
    '/api/v1/answers',
}
assert {'/health','/ready'} <= implemented, sorted(implemented)
undocumented=implemented-({'/health','/ready'}|ALLOWED_IMPLEMENTED)
assert not undocumented, f'implemented without coverage in this validator: {sorted(undocumented)}'
MIGRATIONS=[('000001_initial',22),('000002_account_settings',1),('000003_media_settings',0)]
known=set()
for name,expected in MIGRATIONS:
    up=(ROOT/f'server/migrations/{name}.up.sql').read_text(encoding='utf-8')
    down=(ROOT/f'server/migrations/{name}.down.sql').read_text(encoding='utf-8')
    for sql in (up,down):
        statements=sqlglot.parse(sql,read='mysql',error_level=sqlglot.ErrorLevel.RAISE)
        assert statements and not any(isinstance(x, sqlglot.exp.Command) for x in statements), f'{name} fell back to unparsed command'
    tables=re.findall(r'CREATE TABLE (\w+)',up)
    assert len(tables)==expected and len(set(tables))==expected, f'{name}: {len(tables)} tables'
    assert set(tables)==set(re.findall(r'DROP TABLE (\w+)',down)), f'{name} up/down table mismatch'
    assert 'ON DELETE CASCADE' not in up, name
    assert all(target in tables or target in known for target in re.findall(r'REFERENCES (\w+)',up)), f'{name} references an unknown table'
    known|=set(tables)
print(f'PASS: OpenAPI ({len(operation_ids)} operations), auth contracts, {len(known)} tables across {len(MIGRATIONS)} up/down migrations. MySQL execution is a separate integration check.')

"""End-to-end acceptance walk against a running API.

Every step asserts a status code and, where it matters, that the response says what it should. The point is
not coverage of every branch (the Go suite does that against the real database) but a single pass over the
paths a real deployment must survive: sign in, publish, answer, comment, read, search, report, moderate,
reveal, and be notified.

Usage: acceptance.py <base-url> <origin> <admin-email> <admin-password> <member-email> <member-password>
"""
import http.cookiejar
import json
import sys
import urllib.error
import urllib.parse
import urllib.request

BASE, ORIGIN, ADMIN_EMAIL, ADMIN_PASSWORD, MEMBER_EMAIL, MEMBER_PASSWORD = sys.argv[1:7]

results = []


def check(name, condition, detail=''):
    results.append((name, bool(condition), detail))
    mark = 'PASS' if condition else 'FAIL'
    print(f'  [{mark}] {name}' + (f' — {detail}' if detail and not condition else ''))


class Client:
    """One signed-in account: its own cookie jar, so two accounts cannot share a session by accident."""

    def __init__(self, email, password):
        self.jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))
        self.email = email
        self.csrf = None
        status, body = self.call('POST', '/api/v1/auth/login', {'email': email, 'password': password})
        if status != 200:
            raise SystemExit(f'login failed for {email}: {status} {body}')
        self.csrf = body['csrf_token']

    def call(self, method, path, payload=None):
        data = json.dumps(payload).encode() if payload is not None else None
        request = urllib.request.Request(BASE + path, data=data, method=method)
        # The API enforces an exact Origin on every write, so the harness has to send one.
        request.add_header('Origin', ORIGIN)
        if data is not None:
            request.add_header('Content-Type', 'application/json')
        if self.csrf:
            request.add_header('X-CSRF-Token', self.csrf)
        try:
            with self.opener.open(request) as response:
                raw = response.read().decode()
                return response.status, (json.loads(raw) if raw else None)
        except urllib.error.HTTPError as error:
            raw = error.read().decode()
            try:
                return error.code, json.loads(raw)
            except json.JSONDecodeError:
                return error.code, raw


def main():
    print('1. 登录两个账号（写请求需要 CSRF 令牌）')
    admin = Client(ADMIN_EMAIL, ADMIN_PASSWORD)
    member = Client(MEMBER_EMAIL, MEMBER_PASSWORD)
    check('两个账号都能登录并拿到 CSRF 令牌', bool(admin.csrf) and bool(member.csrf))

    print('2. 发布：讨论帖与提问')
    status, topic = admin.call('POST', '/api/v1/topics', {
        'board_id': '1', 'title': '验收用讨论帖', 'body_md': '验收正文 zzaccept', 'is_anonymous': False, 'tag_ids': [],
    })
    check('管理员发布讨论帖', status == 201, f'{status} {topic}')
    status, question = admin.call('POST', '/api/v1/topics', {
        'board_id': '2', 'title': '验收用提问', 'body_md': '这个功能怎么用？', 'is_anonymous': False, 'tag_ids': [],
    })
    check('管理员发布提问（问答板）', status == 201, f'{status} {question}')

    print('3. 互动：回答、评论、匿名发帖')
    status, answer = member.call('POST', '/api/v1/answers', {
        'topic_id': question['id'], 'body_md': '这样用就行。', 'is_anonymous': False,
    })
    check('成员回答提问', status == 201, f'{status} {answer}')
    status, comment = member.call('POST', '/api/v1/comments', {
        'topic_id': topic['id'], 'body_md': '评论一句。',
    })
    check('成员在讨论帖下评论', status == 201, f'{status} {comment}')
    status, anonymous = member.call('POST', '/api/v1/topics', {
        'board_id': '1', 'title': '验收用匿名帖', 'body_md': '匿名正文。', 'is_anonymous': True, 'tag_ids': [],
    })
    check('成员发布匿名帖', status == 201, f'{status} {anonymous}')

    print('4. 读取与搜索（公开接口，游客可见）')
    guest = Client.__new__(Client)  # a client with no session at all
    guest.jar = http.cookiejar.CookieJar()
    guest.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(guest.jar))
    guest.csrf = None
    status, listing = guest.call('GET', '/api/v1/topics')
    check('游客能读主题列表', status == 200 and listing['items'], f'{status}')
    status, detail = guest.call('GET', f"/api/v1/contents/{topic['id']}")
    check('游客能读单条内容', status == 200 and detail['id'] == topic['id'], f'{status}')
    status, found = guest.call('GET', '/api/v1/search?q=' + urllib.parse.quote('zzaccept'))
    hit = status == 200 and any(item.get('id') == topic['id'] for item in found['items'])
    check('搜索能找到刚发布的内容', hit, f'{status} {found}')

    print('5. 本人内容与公开主页')
    status, mine = admin.call('GET', '/api/v1/me/contents')
    check('本人内容列表包含自己的帖子', status == 200 and any(i.get('id') == topic['id'] for i in mine['items']), f'{status}')
    status, member_own = member.call('GET', '/api/v1/me/contents')
    modes = {i.get('author', {}).get('mode') for i in member_own['items'] if i.get('id') == anonymous['id']}
    check('本人列表包含自己的匿名帖，且以匿名模式渲染', status == 200 and modes == {'anonymous'}, f'{status} {modes}')
    admin_id = member_id = None
    status, users = admin.call('GET', '/api/v1/admin/users')
    for user in users['items']:
        if user['profile']['nickname'] == 'AcceptAdm':
            admin_id = user['internal_user_id']
        if user['profile']['nickname'] == 'AcceptMem':
            member_id = user['internal_user_id']
    check('管理端用户列表可读且能定位账号', status == 200 and admin_id and member_id, f'{status}')
    status, member_profile = guest.call('GET', f"/api/v1/users/{users['items'][0]['profile']['public_id']}")
    check('公开用户主页可读且不含邮箱', status == 200 and 'email' not in json.dumps(member_profile), f'{status}')
    status, member_contents = guest.call('GET', f"/api/v1/users/{[u for u in users['items'] if u['internal_user_id'] == member_id][0]['profile']['public_id']}/contents")
    leaked = any(i.get('id') == anonymous['id'] for i in member_contents['items'])
    check('公开主页不列出该账号的匿名帖', status == 200 and not leaked, f'{status}')

    print('6. 举报与处理闭环')
    status, report = member.call('POST', '/api/v1/reports', {'content_id': topic['id'], 'reason': '验收举报'})
    check('成员提交举报', status == 201, f'{status} {report}')
    status, queue = admin.call('GET', '/api/v1/admin/reports')
    queued = status == 200 and any(item['report']['id'] == report['id'] for item in queue['items'])
    check('举报进入管理队列', queued, f'{status}')
    status, resolved = admin.call('PATCH', f"/api/v1/admin/reports/{report['id']}", {
        'status': 'resolved', 'resolution_note': '验收处理完毕',
    })
    check('管理员结案', status == 200 and resolved['report']['status'] == 'resolved', f'{status}')
    status, own = member.call('GET', '/api/v1/me/reports')
    note = next((i.get('resolution_note') for i in own['items'] if i.get('id') == report['id']), None)
    check('举报者能看到结案说明', status == 200 and note == '验收处理完毕', f'{status} {note}')

    print('7. 审核：隐藏、恢复、锁定、处罚')
    status, _ = admin.call('PATCH', f"/api/v1/admin/contents/{topic['id']}/visibility",
                           {'visibility': 'hidden', 'reason': '验收隐藏'})
    check('隐藏内容返回 204', status == 204, f'{status}')
    status, _ = guest.call('GET', f"/api/v1/contents/{topic['id']}")
    check('隐藏后公开读取 404', status == 404, f'{status}')
    status, _ = admin.call('PATCH', f"/api/v1/admin/contents/{topic['id']}/visibility",
                           {'visibility': 'visible', 'reason': '验收恢复'})
    status2, back = guest.call('GET', f"/api/v1/contents/{topic['id']}")
    check('恢复后公开读取 200', status == 204 and status2 == 200, f'{status} {status2}')
    status, locked = admin.call('PATCH', f"/api/v1/admin/topics/{topic['id']}", {'reason': '验收锁定', 'is_locked': True})
    check('锁定主题', status == 200 and locked['is_locked'] is True, f'{status}')
    status, muted = admin.call('POST', '/api/v1/admin/restrictions',
                               {'user_id': member_id, 'kind': 'mute', 'reason': '验收禁言'})
    check('禁言账号', status == 201 and muted['kind'] == 'mute', f'{status} {muted}')
    status, _ = member.call('POST', '/api/v1/comments', {'topic_id': question['id'], 'body_md': '禁言后不该成功。'})
    check('被禁言后不能新增发言', status in (403, 409, 422), f'{status}')
    status, revoke = admin.call('DELETE', f"/api/v1/admin/restrictions/{muted['id']}")
    check('撤销处罚', status == 204, f'{status}')

    print('8. 站点结构：板块与标签')
    status, board = admin.call('POST', '/api/v1/admin/boards',
                               {'name': '验收板块', 'description': '验收', 'kind': 'discussion', 'sort_order': 99})
    check('新建板块', status == 201, f'{status} {board}')
    status, renamed = admin.call('PUT', f"/api/v1/admin/boards/{board['id']}",
                                 {'name': '验收板块改名', 'description': '验收', 'kind': 'discussion', 'sort_order': 98})
    check('更新板块', status == 200 and renamed['name'] == '验收板块改名', f'{status}')
    status, tag = admin.call('POST', '/api/v1/admin/tags', {'name': '验收标签', 'description': '验收'})
    check('新建标签', status == 201, f'{status} {tag}')
    status, duplicate = admin.call('POST', '/api/v1/admin/tags', {'name': '验收标签', 'description': '重复'})
    check('同名标签返回 409', status == 409, f'{status}')

    print('9. 揭示类操作：删除原文与匿名追溯')
    # The member publishes it and the administrator deletes it: deleting somebody else's content is the path
    # that writes the `admin_delete_content` audit row, and deleting one's own would not exercise it.
    status, disposable = member.call('POST', '/api/v1/topics', {
        'board_id': '1', 'title': '验收用待删帖', 'body_md': '待删原文正文', 'is_anonymous': False, 'tag_ids': [],
    })
    check('成员发布一条待删内容', status == 201, f'{status} {disposable}')
    status, _ = admin.call('DELETE', f"/api/v1/contents/{disposable['id']}")
    check('管理员删除他人内容', status in (200, 204), f'{status}')
    status, archive = admin.call('POST', '/api/v1/admin/deleted-archives/read',
                                 {'content_id': disposable['id'], 'reason': '验收读取原文'})
    check('读取删除原文（含正文）', status == 200 and archive.get('original_body_md') == '待删原文正文', f'{status} {archive}')
    status, _ = admin.call('POST', '/api/v1/admin/deleted-archives/read', {'content_id': disposable['id']})
    check('缺少原因时拒绝', status == 422, f'{status}')
    status, trace = admin.call('POST', '/api/v1/admin/anonymous-traces',
                               {'content_id': anonymous['id'], 'reason': '验收追溯'})
    traced = status == 200 and trace.get('author', {}).get('internal_user_id') == member_id
    check('匿名追溯指向真实作者并给出审计 ID', traced and trace.get('audit_id'), f'{status} {trace}')

    print('10. 通知与审计')
    status, notifications = admin.call('GET', '/api/v1/me/notifications')
    unread = [n for n in notifications['items'] if not n.get('read_at')] if status == 200 else []
    check('回答产生了站内通知', status == 200 and notifications['items'], f'{status}')
    if unread:
        status, _ = admin.call('PUT', f"/api/v1/me/notifications/{unread[0]['id']}/read")
        check('标记通知已读', status in (200, 204), f'{status}')
    status, audit = admin.call('GET', '/api/v1/admin/audit')
    actions = {entry['action'] for entry in audit['items']} if status == 200 else set()
    required = {'hide_content', 'restore_content', 'resolve_report', 'mute_user', 'revoke_restriction',
                'create_board', 'update_board', 'create_tag', 'moderate_topic', 'admin_delete_content',
                'read_deleted_archive', 'anonymous_trace'}
    check('审计覆盖本轮的每个管理动作', status == 200 and required <= actions,
          f'缺失 {sorted(required - actions)}')

    failed = [name for name, ok, _ in results if not ok]
    print()
    print(f'验收检查：{len(results) - len(failed)}/{len(results)} 通过')
    if failed:
        print('失败项：')
        for name in failed:
            print('  - ' + name)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())

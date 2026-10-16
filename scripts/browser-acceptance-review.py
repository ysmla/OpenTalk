"""Independent browser acceptance against an isolated, seeded local environment.

Run with the project validation Python and a live review API/Vite pair.
Only use the disposable accept-member/accept-admin test accounts.
"""
import json
import re
import sys
import struct
import zlib
from pathlib import Path
from playwright.sync_api import sync_playwright, expect

BASE = sys.argv[1] if len(sys.argv) > 1 else 'http://127.0.0.1:15174'
OUT = Path('.cache/browser-acceptance-review')
OUT.mkdir(parents=True, exist_ok=True)
results, errors = [], []

def check(name, ok, detail=''):
    results.append({'name': name, 'pass': bool(ok), 'detail': detail})
    print(('PASS ' if ok else 'FAIL ') + name + (': ' + str(detail) if detail else ''), flush=True)

def visit(page, route):
    page.goto(BASE + '/#/' + route)
    page.wait_for_load_state('networkidle')
    expect(page.get_by_text(re.compile(r'^正在(?:加载|读取|搜索)(?:主题)?…$'))).to_have_count(0)

def login(page, admin=False):
    visit(page, 'login')
    page.get_by_label('邮箱', exact=True).fill('accept-admin@example.test' if admin else 'accept-member@example.test')
    page.get_by_label('密码', exact=True).fill('acceptadmin123' if admin else 'acceptmember123')
    page.get_by_role('button', name='登录', exact=True).click()
    page.wait_for_url('**/#/me')
    page.wait_for_load_state('networkidle')

def main():
    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True)
        context = browser.new_context(viewport={'width': 1365, 'height': 900})
        page = context.new_page()
        page.on('pageerror', lambda error: errors.append(str(error)))
        visit(page, 'topics')
        check('guest topic list', page.get_by_role('heading', name='主题列表', exact=True).is_visible())
        visit(page, 'search?q=zzaccept')
        expect(page.get_by_role('link', name=re.compile('验收用讨论帖')).first).to_be_visible()
        check('guest search returns seeded topic', True)
        login(page)
        me = context.request.get(BASE + '/api/v1/me').json()
        check('browser account is ordinary user', me.get('role') == 'user', me.get('role'))
        check('ordinary user cannot read admin users', context.request.get(BASE + '/api/v1/admin/users').status == 403)
        visit(page, 'new?board_id=2')
        page.get_by_label('标题', exact=True).fill('浏览器匿名问题验收')
        page.get_by_label('正文（Markdown）', exact=True).fill('**浏览器正文**\n\n<script>window.reviewXSS=1</script>\n\n[验收外链](https://example.test/outbound)')
        check('raw HTML preview stays inert', page.evaluate('window.reviewXSS === undefined') and page.locator('.preview script').count() == 0)
        page.get_by_label('以匿名身份发布（发布后不可切换）', exact=True).check()
        with page.expect_response(lambda r: r.request.method == 'POST' and r.url.endswith('/api/v1/topics')) as created:
            page.get_by_role('button', name='发布问题', exact=True).click()
        question = created.value.json()
        check('server rejects raw HTML', created.value.status == 422 and question.get('code') == 'BODY_RAW_HTML')
        if created.value.status != 201:
            page.screenshot(path=str(OUT / 'publish-error.png'), full_page=True)
            page.get_by_label('正文（Markdown）', exact=True).fill('**浏览器正文**\n\n[验收外链](https://example.test/outbound)')
            with page.expect_response(lambda r: r.request.method == 'POST' and r.url.endswith('/api/v1/topics')) as created:
                page.get_by_role('button', name='发布问题', exact=True).click()
            question = created.value.json()
        page.wait_for_url('**/#/topic/*')
        page.wait_for_load_state('networkidle')
        qid = question['id']
        check('anonymous question publish', created.value.status == 201 and question['author']['mode'] == 'anonymous')
        check('question supports direct clarification comments', page.get_by_role('region', name='主题评论', exact=True).count() == 1)
        page.screenshot(path=str(OUT / 'anonymous-question.png'), full_page=True)
        # Intercept the destination to avoid external traffic while proving whether the click leaves the site.
        page.route('https://example.test/**', lambda route: route.fulfill(status=200, body='EXTERNAL DESTINATION'))
        page.get_by_role('link', name='验收外链', exact=True).click()
        page.wait_for_load_state('networkidle')
        check('external link requires intermediate warning', page.url.startswith(BASE), page.url)
        visit(page, 'topic/' + qid)
        page.get_by_label('回答正文（Markdown）', exact=True).fill('匿名楼主的自答')
        with page.expect_response(lambda r: r.request.method == 'POST' and r.url.endswith('/api/v1/answers')) as answered:
            page.get_by_role('button', name='发布回答', exact=True).click()
        answer = answered.value.json()
        page.wait_for_load_state('networkidle')
        check('anonymous owner self-answer forced anonymous', answered.value.status == 201 and answer['author']['mode'] == 'anonymous', answer['author'])
        page.get_by_label('发表评论', exact=True).fill('匿名楼主评论')
        with page.expect_response(lambda r: r.request.method == 'POST' and r.url.endswith('/api/v1/comments')) as commented:
            page.get_by_role('button', name='发布评论', exact=True).click()
        comment = commented.value.json()
        check('anonymous owner comment forced anonymous', commented.value.status == 201 and comment['author']['mode'] == 'anonymous')
        page.wait_for_load_state('networkidle')
        page.get_by_role('button', name='标记为已解决', exact=True).click()
        expect(page.get_by_role('button', name='重新打开问题', exact=True)).to_be_visible()
        page.wait_for_load_state('networkidle')
        check('solved question hides answer and comment composers', page.locator('textarea').count() == 0)
        page.get_by_role('button', name='重新打开问题', exact=True).click()
        expect(page.get_by_label('回答正文（Markdown）', exact=True)).to_be_visible()
        check('reopening restores interaction', True)
        visit(page, 'new?board_id=1')
        page.get_by_label('标题', exact=True).fill('浏览器普通帖子验收')
        page.get_by_label('正文（Markdown）', exact=True).fill('普通用户发布正文')
        def chunk(kind, data):
            return struct.pack('!I', len(data)) + kind + data + struct.pack('!I', zlib.crc32(kind + data) & 0xffffffff)
        png = b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('!2I5B', 2, 2, 8, 2, 0, 0, 0)) + chunk(b'IDAT', zlib.compress((b'\0' + b'\x20\x80\xa0' * 2) * 2)) + chunk(b'IEND', b'')
        with page.expect_response(lambda r: r.request.method == 'POST' and '/attachments' in r.url) as upload:
            page.locator('input[type=file]').set_input_files({'name': 'review.png', 'mimeType': 'image/png', 'buffer': png})
        check('browser image upload', upload.value.status == 201)
        expect(page.get_by_label('正文（Markdown）', exact=True)).to_have_value(re.compile(r'!\['))
        with page.expect_response(lambda r: r.request.method == 'POST' and r.url.endswith('/api/v1/topics')) as posted:
            page.get_by_role('button', name='发布主题', exact=True).click()
        topic = posted.value.json()
        page.wait_for_url('**/#/topic/*')
        page.wait_for_load_state('networkidle')
        check('ordinary discussion publish', posted.value.status == 201)
        expect(page.locator('.topic-body img').first).to_be_visible()
        check('published image loads', page.locator('.topic-body img').first.evaluate('(img) => img.complete && img.naturalWidth > 0'))
        section = page.get_by_role('region', name='主题评论', exact=True)
        check('comment author can choose anonymity', section.get_by_role('checkbox').count() > 0,
              'ordinary topic composer checkbox count=' + str(section.get_by_role('checkbox').count()))
        page.get_by_label('发表评论', exact=True).fill('浏览器评论验收')
        with page.expect_response(lambda r: r.request.method == 'POST' and r.url.endswith('/api/v1/comments')) as normal_comment:
            page.get_by_role('button', name='发布评论', exact=True).click()
        check('ordinary comment submit', normal_comment.value.status == 201)
        comment_id = normal_comment.value.json()['id']
        page.wait_for_load_state('networkidle')
        page.get_by_role('button', name='回复', exact=True).first.click()
        check('reply author can choose anonymity', section.get_by_role('checkbox').count() > 0)
        page.get_by_label('回复', exact=True).fill('浏览器回复验收')
        with page.expect_response(lambda r: r.request.method == 'POST' and r.url.endswith('/api/v1/comments')) as reply:
            page.get_by_role('button', name='发布回复', exact=True).click()
        check('nested reply submit', reply.value.status == 201)
        reply_id = reply.value.json()['id']
        page.wait_for_load_state('networkidle')
        visit(page, 'edit/' + comment_id)
        expect(page.get_by_text('正在加载…', exact=True)).not_to_be_visible()
        check('author can edit comment through editor', page.get_by_label('正文（Markdown）', exact=True).count() == 1, page.locator('main').inner_text())
        page.screenshot(path=str(OUT / 'comment-edit.png'), full_page=True)
        visit(page, 'edit/' + topic['id'])
        page.get_by_label('正文（Markdown）', exact=True).fill('浏览器修改后正文')
        page.get_by_role('button', name='保存修改', exact=True).click()
        page.wait_for_url('**/#/topic/*')
        expect(page.locator('.topic-body')).to_contain_text('浏览器修改后正文')
        check('edit round trip', True)
        page.once('dialog', lambda dialog: dialog.accept())
        with page.expect_response(lambda r: r.request.method == 'DELETE' and r.url.endswith('/contents/' + topic['id'])) as deleted:
            page.get_by_role('button', name='删除', exact=True).first.click()
        check('author deletes own topic', deleted.value.status == 200)
        check('topic deletion removes descendants', all(context.request.get(BASE + '/api/v1/contents/' + cid).status == 404 for cid in [comment_id, reply_id]))
        for route in ['me/contents', 'me/reports', 'me/notifications']:
            visit(page, route)
            check('member page ' + route, page.locator('h1').count() == 1 and page.locator('[role=alert]').count() == 0)
        visit(page, 'me')
        check('profile provides mail preference controls', page.get_by_role('checkbox', name=re.compile('邮件|评论|回答|回复')).count() > 0)
        visit(page, 'unsubscribe?token=review-invalid-token')
        check('unsubscribe route exists rather than falling back to homepage', page.get_by_role('heading', name=re.compile('退订|取消订阅')).count() > 0, page.locator('h1').inner_text())
        visit(page, 'topics')
        page.set_viewport_size({'width': 390, 'height': 844})
        page.screenshot(path=str(OUT / 'mobile-topics.png'), full_page=True)
        check('mobile topic list has no horizontal overflow', page.evaluate('document.documentElement.scrollWidth <= innerWidth'))
        visit(page, 'topic/' + qid)
        check('mobile question has no horizontal overflow', page.evaluate('document.documentElement.scrollWidth <= innerWidth'))
        page.screenshot(path=str(OUT / 'mobile-question.png'), full_page=True)
        admin = browser.new_context(viewport={'width': 1365, 'height': 900})
        ap = admin.new_page()
        ap.on('pageerror', lambda error: errors.append(str(error)))
        login(ap, admin=True)
        for route in ['admin', 'admin/moderation', 'admin/operations']:
            visit(ap, route)
            check('admin page ' + route, ap.locator('h1').count() == 1 and ap.locator('[role=alert]').count() == 0)
            ap.screenshot(path=str(OUT / (route.replace('/', '-') + '.png')), full_page=True)
        check('no uncaught browser exceptions', not errors, errors)
        browser.close()

try:
    main()
except Exception as error:
    check('harness completed', False, str(error))
    raise
finally:
    (OUT / 'results.json').write_text(json.dumps(results, ensure_ascii=False, indent=2), encoding='utf-8')
    print('BROWSER_RESULTS', sum(r['pass'] for r in results), '/', len(results), flush=True)
if any(not result['pass'] for result in results):
    sys.exit(1)

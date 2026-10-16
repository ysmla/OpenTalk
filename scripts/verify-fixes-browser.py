"""Browser verification of the six repaired flows from docs/full-acceptance-2026-10-15.md.

Run against an isolated review API/Vite pair, e.g.

    .cache/validation/Scripts/python.exe scripts/verify-fixes-browser.py http://127.0.0.1:15174

Every account used here is a disposable review account and the environment it
runs against is a scratch database, never a real one.
"""
import json
import os
import re
import sys
from pathlib import Path
from playwright.sync_api import sync_playwright, expect

# The token minter is a sibling verification fixture, imported by filename.
sys.path.insert(0, str(Path(__file__).resolve().parent))
from mint_unsubscribe_token import mint as mint_unsubscribe_token

BASE = sys.argv[1] if len(sys.argv) > 1 else 'http://127.0.0.1:15174'
OUT = Path('.cache/browser-fixes-verification')
OUT.mkdir(parents=True, exist_ok=True)
results, errors = [], []
ADMIN_EMAIL, ADMIN_PASSWORD = 'verify-admin@example.test', 'verifyadmin1'
MEMBER_EMAIL, MEMBER_PASSWORD = 'verify-member@example.test', 'verifymember1'


def member_internal_id(admin_page, member_public_id: str) -> str:
    """Translate the member's public id into the internal id the token names.

    The admin user list is the only endpoint that speaks internal ids, so the
    fixture reads it once, through the administrator session it already has.
    """
    body = admin_page.evaluate(
        """async (publicId) => {
            const response = await fetch('/api/v1/admin/users?limit=50', { credentials: 'same-origin' });
            if (!response.ok) return null;
            const page = await response.json();
            const hit = (page.items || []).find(user => user.profile && user.profile.public_id === publicId);
            return hit ? String(hit.internal_user_id) : null;
        }""",
        member_public_id,
    )
    return body or ''


def check(name, ok, detail=''):
    results.append({'name': name, 'pass': bool(ok), 'detail': str(detail)})
    print(('PASS ' if ok else 'FAIL ') + name + (': ' + str(detail) if detail else ''), flush=True)


def settle(page):
    """Wait until the current screen has finished its own reads.

    Waiting for "networkidle" is not enough: the page sets its hash first and
    renders the loader afterwards, so a screen can be idle while it still shows
    正在加载…. Every visible loader here starts with 正在 and ends with …; the
    screens clear theirs once their read has settled, which is the signal the
    assertions below need. `main` is waited for as well so the ready state always
    means a rendered screen.
    """
    page.wait_for_load_state('networkidle')
    expect(page.locator('main')).to_be_visible()
    expect(page.get_by_text(re.compile(r'^正在.*…$'))).to_have_count(0)


def visit(page, route):
    """Open one screen and wait for it to settle."""
    page.goto(BASE + '/#/' + route)
    settle(page)


def login(page, email, password):
    visit(page, 'login')
    page.get_by_label('邮箱', exact=True).fill(email)
    page.get_by_label('密码', exact=True).fill(password)
    page.get_by_role('button', name='登录', exact=True).click()
    page.wait_for_url('**/#/me')
    page.wait_for_load_state('networkidle')


def main():
    # The scratch API's own APP_SECRET, needed to sign one valid unsubscribe
    # token; it is read from the environment and never written into the report.
    secret = os.environ.get('APP_SECRET', '')
    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True)
        context = browser.new_context(viewport={'width': 1365, 'height': 900})
        page = context.new_page()
        page.on('pageerror', lambda error: errors.append(str(error)))
        login(page, 'verify-member@example.test', 'verifymember1')
        # 正文里四种链接形态各一条：锚点、相对路径、同源绝对地址、站外地址。
        # 前三种必须直达（站内链接直达），只有第四种允许出现确认对话框。
        same_origin = BASE + '/#/topics'

        # ---------------------------------------------------------------- 问题
        visit(page, 'new?board_id=2')
        page.get_by_label('标题', exact=True).fill('修复复验问题')
        page.get_by_label('正文（Markdown）', exact=True).fill(
            '锚点链接：[锚点站内](#/topics)\n\n'
            '相对路径：[相对站内](/health)\n\n'
            f'同源绝对地址：[绝对站内]({same_origin})\n\n'
            '站外链接：[验收外链](https://example.test/outbound)')
        with page.expect_response(lambda r: r.request.method == 'POST' and r.url.endswith('/api/v1/topics')) as created:
            page.get_by_role('button', name='发布问题', exact=True).click()
        question = created.value.json()
        page.wait_for_url('**/#/topic/*')
        settle(page)
        qid = question['id']

        # 问题 4：问题本身要有澄清评论入口（无回答时也在）。
        question_comments = page.get_by_role('region', name='主题评论', exact=True)
        check('问题本身有澄清评论区', question_comments.count() == 1)
        check('无回答时问题评论框可用', question_comments.get_by_label('发表评论', exact=True).count() == 1)

        # 问题 2：普通参与者的评论与回复都能逐条选择匿名。
        composer_checkbox = question_comments.get_by_role('checkbox')
        check('普通评论有匿名开关', composer_checkbox.count() == 1, composer_checkbox.count())
        composer_checkbox.first.check()
        page.route('https://example.test/**', lambda route: route.fulfill(status=200, body='EXTERNAL DESTINATION'))
        question_comments.get_by_label('发表评论', exact=True).fill('普通用户匿名评论')
        with page.expect_response(lambda r: r.request.method == 'POST' and r.url.endswith('/api/v1/comments')) as first_comment:
            page.get_by_role('button', name='发布评论', exact=True).click()
        sent = json.loads(first_comment.value.request.post_data)
        check('评论按开关提交 is_anonymous=true', first_comment.value.status == 201 and sent.get('is_anonymous') is True, sent)
        check('匿名评论以匿名身份返回', first_comment.value.json()['author']['mode'] == 'anonymous')
        comment_id = first_comment.value.json()['id']
        page.wait_for_load_state('networkidle')

        # 回复同样可以逐条选择匿名（刚才的开关在切换回复后需要重新勾选）。
        question_comments.get_by_role('button', name='回复', exact=True).first.click()
        check('回复表单有匿名开关', question_comments.get_by_role('checkbox').count() == 1)
        question_comments.get_by_role('checkbox').first.check()
        question_comments.get_by_label('回复', exact=True).fill('普通用户匿名回复')
        with page.expect_response(lambda r: r.request.method == 'POST' and r.url.endswith('/api/v1/comments')) as reply:
            page.get_by_role('button', name='发布回复', exact=True).click()
        reply_sent = json.loads(reply.value.request.post_data)
        check('回复按开关提交 is_anonymous=true', reply.value.status == 201 and reply_sent.get('is_anonymous') is True, reply_sent)
        reply_id = reply.value.json()['id']
        page.wait_for_load_state('networkidle')

        # 问题 1：外链先显示真实域名与继续访问，站内链接直达。
        # 断言一律限定在正文容器 `.topic-body` 内：面包屑与侧边导航不经过
        # Markdown 拦截器，点它们不能证明正文里的链接行为。
        body = page.locator('.topic-body')
        expect(body.get_by_role('link', name='锚点站内', exact=True)).to_be_visible()

        # 站内三种形态都不得出现对话框。
        body.get_by_role('link', name='锚点站内', exact=True).click()
        page.wait_for_url('**/#/topics')
        check('正文锚点链接直达', page.get_by_role('dialog').count() == 0 and page.url.endswith('#/topics'), page.url)
        visit(page, 'topic/' + qid)

        with page.expect_navigation():
            page.locator('.topic-body').get_by_role('link', name='相对站内', exact=True).click()
        check('正文相对路径直达', page.get_by_role('dialog').count() == 0 and page.url.startswith(BASE), page.url)
        visit(page, 'topic/' + qid)

        body = page.locator('.topic-body')
        with page.expect_navigation():
            body.get_by_role('link', name='绝对站内', exact=True).click()
        check('正文同源绝对地址直达', page.get_by_role('dialog').count() == 0 and page.url.endswith('#/topics'), page.url)
        visit(page, 'topic/' + qid)
        body = page.locator('.topic-body')
        body.get_by_role('link', name='验收外链', exact=True).click()
        dialog = page.get_by_role('dialog')
        expect(dialog).to_be_visible()
        check('外链未立即离开本站', page.url.startswith(BASE), page.url)
        check('中间页展示真实域名', dialog.get_by_text('example.test', exact=True).count() == 1, dialog.inner_text())
        page.screenshot(path=str(OUT / 'outbound-confirm.png'), full_page=True)
        dialog.get_by_role('button', name='取消', exact=True).click()
        expect(dialog).to_have_count(0)
        check('取消后停留在帖子页', page.url.startswith(BASE + '/#/topic/'), page.url)
        body.get_by_role('link', name='验收外链', exact=True).click()
        with page.expect_navigation():
            page.get_by_role('dialog').get_by_role('button', name='继续访问', exact=True).click()
        check('确认后才离开本站', page.url.startswith('https://example.test/'), page.url)
        visit(page, 'topic/' + qid)

        # 举报入口：主题（上面已验）、回答、评论与回复都必须有。
        # 回答先发一条，否则回答区是空的。
        page.get_by_label('回答正文（Markdown）', exact=True).fill('用于验证举报入口的回答')
        with page.expect_response(lambda r: r.request.method == 'POST' and r.url.endswith('/api/v1/answers')) as answered:
            page.get_by_role('button', name='发布回答', exact=True).click()
        check('发布一条回答（为举报入口用）', answered.value.status == 201, answered.value.status)
        settle(page)

        answer_article = page.locator('article.answer').first
        expect(answer_article.get_by_role('button', name='举报', exact=True)).to_be_visible()
        check('回答有举报入口', answer_article.get_by_role('button', name='举报', exact=True).count() == 1)

        # 点开表单并真的提交一次，证明入口不只是摆着。
        answer_article.get_by_role('button', name='举报', exact=True).click()
        expect(page.get_by_label(re.compile('^举报原因'), exact=False)).to_be_visible()
        page.get_by_label(re.compile('^举报原因'), exact=False).fill('验收：验证回答举报入口')
        with page.expect_response(lambda r: r.request.method == 'POST' and r.url.endswith('/api/v1/reports')) as reported:
            page.get_by_role('button', name='提交举报', exact=True).click()
        check('回答举报提交成功', reported.value.status == 201, reported.value.status)
        check('提交后显示已提交提示', page.get_by_text(re.compile('已提交举报'), exact=False).count() >= 1)

        # 问题 3：评论作者可以进入编辑器、保存并回到讨论。
        visit(page, 'edit/' + comment_id)
        check('评论编辑页有正文输入框', page.get_by_label('正文（Markdown）', exact=True).count() == 1, page.locator('main').inner_text()[:200])
        page.get_by_label('正文（Markdown）', exact=True).fill('普通用户匿名评论（已编辑）')
        with page.expect_response(lambda r: r.request.method == 'PATCH' and r.url.endswith('/contents/' + comment_id)) as patched:
            page.get_by_role('button', name='保存修改', exact=True).click()
        check('评论保存成功', patched.value.status == 200, patched.value.status)
        page.wait_for_url('**/#/topic/' + qid)
        expect(page.locator('.comment').first).to_contain_text('已编辑')
        check('保存后回到所属讨论', True, page.url)
        check('编辑后的正文可见', page.get_by_text('普通用户匿名评论（已编辑）', exact=True).count() == 1)

        # 评论与回复同样要有举报入口（此时文案已是"（已编辑）"）。
        # 断言限定在该条评论自己的 .comment-actions 里：li.comment 包含嵌套回复，
        # 不限定就会同时匹配到回复上的那个举报按钮。
        comment_row = page.locator('li.comment').filter(has_text='普通用户匿名评论（已编辑）').first
        comment_actions = comment_row.locator('.comment-actions').first
        expect(comment_actions.get_by_role('button', name='举报', exact=True)).to_be_visible()
        check('评论有举报入口', comment_actions.get_by_role('button', name='举报', exact=True).count() == 1)

        reply_row = page.locator('li.comment.reply').first
        reply_actions = reply_row.locator('.comment-actions').first
        expect(reply_actions.get_by_role('button', name='举报', exact=True)).to_be_visible()
        check('回复有举报入口', reply_actions.get_by_role('button', name='举报', exact=True).count() == 1)

        # 越权与时限仍有效：另一个账号打开这条评论的编辑页应被拒绝。
        # It has to be a *separate browser context*: signing the administrator in
        # inside the member's context replaces the shared session cookie, after
        # which every member write carries the administrator's session with the
        # member's CSRF token — a self-inflicted CSRF_REJECTED. The member's own
        # public id is read from its session, before any of this.
        member_public_id = page.evaluate(
            "async () => (await (await fetch('/api/v1/me', { credentials: 'same-origin' })).json()).profile.public_id")
        admin_context = browser.new_context(viewport={'width': 1365, 'height': 900})
        admin_page = admin_context.new_page()
        admin_page.on('pageerror', lambda error: errors.append(str(error)))
        login(admin_page, ADMIN_EMAIL, ADMIN_PASSWORD)
        visit(admin_page, 'edit/' + comment_id)
        check('他人不能编辑该评论', admin_page.get_by_text('当前账号无法编辑该内容。', exact=True).count() == 1,
              admin_page.locator('main').inner_text()[:120])
        member_id = member_internal_id(admin_page, member_public_id)
        admin_context.close()
        check('能从管理端用户列表定位成员内部 id', member_id.isdigit(), member_id)

        # 问题 4（后半）：问题已解决后问题评论、回答、回答下评论一起关闭。
        page.get_by_role('button', name='标记为已解决', exact=True).click()
        expect(page.get_by_role('button', name='重新打开问题', exact=True)).to_be_visible()
        page.wait_for_load_state('networkidle')
        check('已解决后没有任何输入框', page.locator('textarea').count() == 0, page.locator('textarea').count())
        page.get_by_role('button', name='重新打开问题', exact=True).click()
        # 解决状态决定 can_create_*，界面要重新读一次权限；等这一轮读完之后再断言，
        # 否则看到的是"上一次权限尚未被替换"的瞬间。
        page.wait_for_load_state('networkidle')
        expect(page.get_by_role('button', name='标记为已解决', exact=True)).to_be_visible()
        page.wait_for_timeout(500)
        check('重新打开后问题评论恢复', page.get_by_label('发表评论', exact=True).count() >= 1,
              page.locator('main').inner_text()[:200])

        # 问题 5：邮件偏好可读写且刷新后保持；关闭邮件不影响站内通知。
        # The preconditions are read from the server instead of assumed: whether a new
        # account starts with any mail flag on is the server's decision (and the
        # contract and the account-creation defaults currently disagree about it —
        # see the report), which is not this run's subject.
        prior = context.request.get(BASE + '/api/v1/me/notification-preferences').json()
        print('  prefs before:', prior, flush=True)
        visit(page, 'me')
        boxes = page.get_by_role('checkbox', name=re.compile('收到评论|收到回答|收到回复'))
        check('/me 有三项邮件偏好控件', boxes.count() == 3, boxes.count())
        names = [boxes.nth(i).evaluate("el => el.closest('label').innerText") for i in range(boxes.count())]
        shown = [boxes.nth(i).is_checked() for i in range(boxes.count())]
        check('界面按服务端读到的值渲染', shown == [prior['email_on_comment'], prior['email_on_answer'], prior['email_on_reply']],
              f'{names} -> {shown}')
        # 只改一项：另外两项必须保持服务端的原值。
        target = 2 if prior['email_on_reply'] is False else 0
        want = list(shown)
        want[target] = not want[target]
        with page.expect_response(lambda r: r.request.method == 'PUT' and r.url.endswith('/me/notification-preferences')) as pref:
            boxes.nth(target).set_checked(want[target])
            page.get_by_role('button', name='保存邮件偏好', exact=True).click()
        sent = json.loads(pref.value.request.post_data)
        expected = {
            'email_on_comment': want[0],
            'email_on_answer': want[1],
            'email_on_reply': want[2],
        }
        check('邮件偏好提交三项且只改选定的一项', pref.value.status == 200 and sent == expected, sent)
        visit(page, 'me')
        check('刷新后邮件偏好保持',
              [page.get_by_role('checkbox', name=re.compile('收到评论|收到回答|收到回复')).nth(i).is_checked() for i in range(3)]
              == [want[0], want[1], want[2]])

        # 关闭全部邮件偏好，并确认站内通知不受影响。
        for i in range(3):
            page.get_by_role('checkbox', name=re.compile('收到评论|收到回答|收到回复')).nth(i).set_checked(False)
        with page.expect_response(lambda r: r.request.method == 'PUT' and r.url.endswith('/me/notification-preferences')) as cleared:
            page.get_by_role('button', name='保存邮件偏好', exact=True).click()
        check('关闭全部邮件偏好', cleared.value.status == 200, cleared.value.request.post_data)
        check('服务端记录的偏好已全关',
              context.request.get(BASE + '/api/v1/me/notification-preferences').json()
              == {'email_on_comment': False, 'email_on_answer': False, 'email_on_reply': False})
        visit(page, 'me/notifications')
        check('关闭邮件不影响站内通知页', page.locator('h1').count() == 1 and page.locator('[role=alert]').count() == 0)

        # 问题 5（续）：详情页单篇订阅继承/开启/关闭，刷新后保持。
        visit(page, 'topic/' + qid)
        radios = page.get_by_role('radio')
        check('详情页有单篇邮件设置', radios.count() == 3, radios.count())
        check('未设置读作跟随全局', page.get_by_role('radio', name=re.compile('跟随全局')).is_checked())
        with page.expect_response(lambda r: r.request.method == 'PUT' and '/me/subscriptions/' in r.url) as sub:
            page.get_by_role('radio', name=re.compile('不发邮件')).check()
            page.get_by_role('button', name='保存这个主题的设置', exact=True).click()
        check('单篇设置 topic_id 与路径一致', sub.value.status == 200 and
              json.loads(sub.value.request.post_data)['topic_id'] == qid, sub.value.request.post_data)
        visit(page, 'topic/' + qid)
        check('刷新后单篇设置保持', page.get_by_role('radio', name=re.compile('不发邮件')).is_checked())

        # 问题 6：退订页存在，GET 不退订，失效令牌有明确提示。
        # 用真实签名令牌验证有效路径：令牌按服务端的格式现场签发
        # （scripts/mint_unsubscribe_token.py，密钥来自运行环境的 APP_SECRET）。
        token = mint_unsubscribe_token(secret, member_id, qid) if secret and member_id else ''
        if token is None:
            token = ''
        if token == '':
            check('签发了可用的退订令牌', False, 'APP_SECRET 未设置或成员 id 未知')
        else:
            check('签发了可用的退订令牌', True)
            visit(page, 'unsubscribe?token=' + token)
            check('有效令牌显示确认页', page.get_by_role('heading', name='取消邮件订阅').count() == 1
                  and page.get_by_role('button', name=re.compile('确认退订')).count() == 1)
            page.screenshot(path=str(OUT / 'unsubscribe-confirm.png'), full_page=True)
            # GET 打开不得自动退订：设置仍是上一节保存的"不发邮件"。
            before = context.request.get(BASE + '/api/v1/me/subscriptions/' + qid).json()
            check('GET 打开退订页不改设置', before['email_override'] == 'disabled', before)
            with page.expect_response(lambda r: r.request.method == 'POST' and r.url.endswith('/subscriptions/unsubscribe')) as done:
                page.get_by_role('button', name=re.compile('确认退订')).click()
            unsubscribe_status = done.value.status
            expect(page.get_by_text(re.compile('已退订'), exact=False).first).to_be_visible()
            after = context.request.get(BASE + '/api/v1/me/subscriptions/' + qid).json()
            check('点击后 POST 退订成功', unsubscribe_status == 204, unsubscribe_status)
            # 取消订阅删除该主题的设置行，因此读回 inherit：这一篇回到跟随全局，而不是被钉在
            # disabled。这一步之前设置是 disabled，所以值的改变本身就是写入生效的证据。
            check('退订删除该主题的设置行（读回 inherit）', after['email_override'] == 'inherit', after)
            prefs = context.request.get(BASE + '/api/v1/me/notification-preferences').json()
            check('退订不改动全局偏好',
                  prefs == {'email_on_comment': False, 'email_on_answer': False, 'email_on_reply': False}, prefs)
        visit(page, 'unsubscribe?token=review-invalid-token')
        check('退订路由不再回落首页', page.get_by_role('heading', name='取消邮件订阅').count() == 1, page.locator('h1').inner_text())
        # 打开页面本身不验证令牌、也不改状态：GET 不写任何东西，令牌只在用户点击后
        # 交给服务端校验。因此这里确认的是"没有假成功"，以及点击后拿到明确提示。
        check('无效令牌不显示成功', page.get_by_text(re.compile('已退订'), exact=False).count() == 0)
        visits_before = json.dumps(context.request.get(BASE + '/api/v1/me/subscriptions/' + qid).json())
        with page.expect_response(lambda r: r.request.method == 'POST' and r.url.endswith('/subscriptions/unsubscribe')) as bad:
            page.get_by_role('button', name=re.compile('确认退订')).click()
        check('无效令牌被服务端拒绝', bad.value.status == 400, bad.value.status)
        check('无效令牌有明确提示', page.get_by_text('这个退订链接无效或已过期。', exact=True).count() == 1,
              page.locator('main').inner_text()[:200])
        page.wait_for_timeout(300)
        check('失效令牌不触发任何退订写入',
              visits_before == json.dumps(context.request.get(BASE + '/api/v1/me/subscriptions/' + qid).json()))
        visit(page, 'unsubscribe')
        check('缺少令牌也有明确提示', page.get_by_text('这个退订链接无效或已过期。', exact=True).count() == 1)

        check('没有未捕获的浏览器异常', not errors, errors)
        browser.close()


try:
    main()
except Exception as error:
    check('harness completed', False, repr(error))
    raise
finally:
    (OUT / 'results.json').write_text(json.dumps(results, ensure_ascii=False, indent=2), encoding='utf-8')
    print('BROWSER_RESULTS', sum(r['pass'] for r in results), '/', len(results), flush=True)

if any(not result['pass'] for result in results):
    sys.exit(1)

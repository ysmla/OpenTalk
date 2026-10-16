"""Run repair suite plus independent internal-link and effective unsubscribe checks.
Use only with the disposable environment from verify-repairs.ps1 -Probe.
"""
import json
import os
from pathlib import Path
import runpy
import sys
from playwright.sync_api import sync_playwright, expect
sys.path.insert(0,str(Path(__file__).resolve().parent))
from mint_unsubscribe_token import mint

runpy.run_path(str(Path(__file__).with_name('verify-fixes-browser.py')),run_name='__main__')
base=sys.argv[1]
results=[]
def check(name,ok,detail=''):
    results.append({'name':name,'pass':bool(ok),'detail':detail})
    print(('PASS ' if ok else 'FAIL ')+name+': '+str(detail),flush=True)
def visit(p,route):
    p.goto(base+'/#/'+route)
    p.wait_for_load_state('networkidle')
    expect(p.get_by_text(__import__('re').compile(r'^正在.*…$'))).to_have_count(0)
def login(p,email,password):
    visit(p,'login')
    p.get_by_label('邮箱',exact=True).fill(email)
    p.get_by_label('密码',exact=True).fill(password)
    p.get_by_role('button',name='登录',exact=True).click()
    p.wait_for_url('**/#/me')
    p.wait_for_load_state('networkidle')
with sync_playwright() as pw:
    browser=pw.chromium.launch(headless=True)
    member=browser.new_context()
    page=member.new_page()
    login(page,'verify-member@example.test','verifymember1')
    me=member.request.get(base+'/api/v1/me').json()
    check('member really has user role',me['role']=='user')
    items=member.request.get(base+'/api/v1/topics').json()['items']
    qid=next(x['id'] for x in items if x.get('title')=='修复复验问题')
    visit(page,'topic/'+qid)
    # The body link is looked up by the same label the repair suite uses for the
    # fragment case ("锚点站内"); the previous label ("主题列表") belonged to the
    # content this script was written against and no longer exists after the
    # in-body link checks started covering all four link shapes.
    page.locator('.topic-body').get_by_role('link',name='锚点站内',exact=True).click()
    page.wait_for_load_state('networkidle')
    check('Markdown internal fragment navigates without leave-site dialog',page.url==base+'/#/topics' and page.get_by_role('dialog').count()==0,{'url':page.url,'dialogs':page.get_by_role('dialog').count()})
    page.screenshot(path='.cache/browser-fixes-verification/internal-link-recheck.png',full_page=True)
    if page.get_by_role('dialog').count():
        page.get_by_role('dialog').get_by_role('button',name='取消',exact=True).click()
    token=member.request.get(base+'/api/v1/auth/csrf').json()['csrf_token']
    headers={'Origin':base,'X-CSRF-Token':token}
    prefs={'email_on_comment':True,'email_on_answer':True,'email_on_reply':True}
    assert member.request.put(base+'/api/v1/me/notification-preferences',data=prefs,headers=headers).status==200
    assert member.request.put(base+'/api/v1/me/subscriptions/'+qid,data={'topic_id':qid,'email_override':'enabled'},headers=headers).status==200
    admin=browser.new_context()
    ap=admin.new_page()
    login(ap,'verify-admin@example.test','verifyadmin1')
    users=admin.request.get(base+'/api/v1/admin/users').json()['items']
    uid=next(x['internal_user_id'] for x in users if x['profile']['public_id']==me['profile']['public_id'])
    signed=mint(os.environ['APP_SECRET'],str(uid),qid)
    guest=browser.new_context()
    gp=guest.new_page()
    visit(gp,'unsubscribe?token='+signed)
    before=member.request.get(base+'/api/v1/me/subscriptions/'+qid).json()
    check('GET unsubscribe leaves enabled subscription untouched',before['email_override']=='enabled',before)
    with gp.expect_response(lambda r:r.request.method=='POST' and r.url.endswith('/subscriptions/unsubscribe')) as res:
        gp.get_by_role('button',name='确认退订这个主题的邮件',exact=True).click()
    after=member.request.get(base+'/api/v1/me/subscriptions/'+qid).json()
    # Cancelling clears the per-topic row (it reads back as `inherit`), so the check is that the
    # logged-out confirmation removed the `enabled` choice instead of pinning the topic to `disabled`.
    check('logged-out confirmation clears an enabled subscription',res.value.status==204 and after['email_override']=='inherit',{'status':res.value.status,'after':after})
    check('unsubscribe preserves enabled global preferences',member.request.get(base+'/api/v1/me/notification-preferences').json()==prefs)
    browser.close()
Path('.cache/browser-fixes-verification/boundary-results.json').write_text(json.dumps(results,ensure_ascii=False,indent=2),encoding='utf-8')
print('BOUNDARY_RESULTS',sum(x['pass'] for x in results),'/',len(results),flush=True)
sys.exit(1 if any(not x['pass'] for x in results) else 0)

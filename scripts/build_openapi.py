"""Build the local REST contract from the canonical component models."""
import json
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT / 'docs/api/models.openapi.json'
spec = json.loads(SOURCE.read_text(encoding='utf-8'))
spec['info'].update(title='OpenTalk REST API', version='0.5.0', description='本地维护的接口合同。x-implementation-status标记每个操作是否已实现；标记为planned的操作不代表已提供业务能力。写操作必须检查 Origin，登录后的写操作另需会话绑定的 CSRF token。')
spec['servers'] = [{'url': '/', 'description': '同源部署；API路径已含/api/v1前缀'}]
schemas = spec['components']['schemas']
ref = lambda name: {'$ref': '#/components/schemas/' + name}

def model(name, properties, required=None, description=''):
    schemas[name] = {'type': 'object', 'additionalProperties': False, 'description': description,
                     'properties': properties, 'required': list(properties) if required is None else required}

def page(name, item):
    model(name, {'items': {'type': 'array', 'items': ref(item)}, 'has_more': {'type': 'boolean'},
                 'next_cursor': {'type': 'string'}}, ['items', 'has_more'], '游标分页；没有下一页时省略next_cursor。')

model('HealthStatus', {'status': {'type': 'string', 'enum': ['ok']}})
model('AuthSession', {'user': ref('CurrentUser'), 'csrf_token': {'type': 'string'}}, description='Cookie设置随机会话凭证；csrf_token仅在当前前端内存保存，不能写日志或放URL。')
model('CSRFToken', {'csrf_token': {'type': 'string'}}, description='仅当前会话读取；Cache-Control: no-store；根据原始会话凭证和服务端密钥确定性派生，数据库仅存摘要，多个标签页读取不轮换。')
model('AdminArchiveRequest', {'content_id': {'type': 'string', 'pattern': '^[1-9][0-9]*$'}, 'reason': {'type': 'string', 'minLength': 1}}, description='读取原文前记录访问原因与审计，30天到期后不可读取。')
model('UnsubscribeRequest', {'token': {'type': 'string', 'minLength': 1, 'writeOnly': True}}, description='邮件中的签名退订凭证，只能取消该邮件对应的通知范围，不能登录或访问内容；服务端校验签名、范围和有效期。')
for name, item in [('BoardPage','Board'),('TagPage','Tag'),('NotificationPage','Notification'),('MyReportPage','MyReport'),('AdminReportPage','AdminReport'),('AdminUserPage','AdminUser'),('AdminRestrictionPage','AdminRestriction'),('AdminAuditPage','AdminAuditEntry')]:
    page(name, item)
spec['components']['securitySchemes'] = {
    'SessionCookie': {'type':'apiKey','in':'cookie','name':'opentalk_session','description':'HttpOnly、SameSite=Lax；生产环境Secure；服务端可撤销。'},
    'CSRFToken': {'type':'apiKey','in':'header','name':'X-CSRF-Token','description':'登录后写操作必须与会话绑定的token匹配，另要求精确Origin。'},
}
spec['paths'] = {}
def query(name, schema, description=''):
    return {'name':name,'in':'query','required':False,'description':description,'schema':schema}

def operation(method, path, operation_id, summary, response=None, request=None, auth='user', status='200', description='', params=None, implemented=False):
    full = path if path in ('/health','/ready','/media/{key}') else '/api/v1' + path
    parameters = [{'name': name, 'in':'path','required':True,'schema':{'type':'string','minLength':1}} for name in re.findall(r'{(\w+)}',full)]
    parameters += params or []
    responses = {status: {'description': summary + '成功'}}
    if response:
        responses[status]['content'] = {'application/json': {'schema': ref(response)}}
    for code, meaning in {'400':'请求格式或游标无效','401':'未登录或会话过期','403':'权限、Origin或CSRF检查失败','404':'资源不存在或不可见','409':'状态冲突（已解决、锁定、删除或重复未结案举报）','413':'请求体过大','422':'字段或业务参数无效','429':'请求过于频繁','500':'内部错误','503':'依赖不可用'}.items():
        responses[code] = {'description': meaning, 'content': {'application/json': {'schema':ref('ApiError')}}}
    security = [] if auth == 'public' else [{'SessionCookie': [], **({'CSRFToken': []} if method not in ('get','head') else {})}]
    op = {'operationId':operation_id,'summary':summary,'description':description or summary,
          'tags':['运维' if path in ('/health','/ready') else ('管理' if auth=='admin' else path.strip('/').split('/')[0])],
          'security':security,'responses':responses,'x-implementation-status':'implemented' if implemented else 'planned'}
    if auth == 'admin':
        op['description'] += '\n需要管理员权限；涉及匿名追溯与原文读取时需专用审计。'
        op['x-required-role'] = 'admin'
    if parameters: op['parameters'] = parameters
    if request: op['requestBody'] = {'required':True,'content':{'application/json':{'schema':ref(request)}}}
    spec['paths'].setdefault(full,{})[method]=op
    return op

pagination=[query('cursor',{'type':'string'},'绑定查询过滤和排序条件的不透明游标'),query('limit',{'type':'integer','minimum':1,'maximum':100,'default':20})]
operation('get','/health','getHealth','检查进程存活','HealthStatus',auth='public',implemented=True)
operation('get','/ready','getReady','检查MySQL、Redis和迁移版本','HealthStatus',auth='public',implemented=True)
operation('get','/auth/captcha','getCaptcha','获取图形验证码','CaptchaChallenge',auth='public',params=[query('purpose',{'type':'string','enum':['register','reset_password','login']})],implemented=True)
# The stored challenge is bound to one purpose, so the server must not guess a default.
spec['paths']['/api/v1/auth/captcha']['get']['parameters'][0]['required']=True
op=operation('get','/auth/captcha/{challenge_id}/image','getCaptchaImage','读取图形验证码图片',auth='public',description='短期挑战图片，不返回答案；禁止缓存。',implemented=True)
op['responses']['200']['content']={'image/png':{'schema':{'type':'string','format':'binary'}}}
operation('post','/auth/email-codes','sendEmailCode','发送邮件验证码','EmailCodeChallenge','SendEmailCodeRequest',auth='public',status='202',description='图形验证码先校验再发送。按邮箱、IP、用途限流；注册状态不通过响应泄露。',implemented=True)
operation('post','/auth/register','register','邮箱注册',request='RegisterRequest',auth='public',status='204',description='注册成功后由用户登录，不自动创建会话。',implemented=True)
op=operation('post','/auth/login','login','登录','AuthSession','LoginRequest',auth='public',implemented=True)
op['responses']['200']['headers']={'Set-Cookie':{'description':'opentalk_session，HttpOnly；生产环境Secure；SameSite=Lax；Path=/。','schema':{'type':'string'}}}
operation('post','/auth/logout','logout','退出登录',status='204',description='撤销当前会话并清除Cookie。',implemented=True)
operation('post','/auth/reset-password','resetPassword','验证码重置密码',request='ResetPasswordRequest',auth='public',status='204',description='消费验证码并重置密码；撤销所有既有会话。',implemented=True)
operation('get','/auth/csrf','getCSRF','获取当前会话CSRF令牌','CSRFToken',implemented=True)
operation('get','/me','getMe','读取本人资料','CurrentUser',implemented=True)
operation('patch','/me','updateMe','修改本人资料','CurrentUser','UpdateProfileRequest',implemented=True)
operation('get','/me/contents','listMyContents','读取本人内容','ContentPage',params=pagination,description='私有列表允许作者查看自己的匿名内容；仍不返回删除原文。隐藏处置可通过通知查看。')
operation('get','/users/{public_id}','getPublicUser','读取公开用户资料','PublicUser',auth='public')
operation('get','/users/{public_id}/contents','listUserContents','读取用户公开内容','ContentPage',auth='public',params=pagination,description='必须排除匿名、隐藏、删除内容；禁止跨身份关联。')
operation('get','/boards','listBoards','读取板块','BoardPage',auth='public',params=pagination,implemented=True)
operation('get','/tags','listTags','搜索标签','TagPage',auth='public',params=pagination+[query('q',{'type':'string'})],description='按名称排序的公开列表，游标绑定搜索文本；q按名称子串匹配，其中的LIKE通配符按字面转义。',implemented=True)
operation('get','/topics','listTopics','读取帖子和问题','ContentPage',auth='public',params=pagination+[query('board_id',{'type':'string'}),query('tag_id',{'type':'string'}),query('resolution_status',{'type':'string','enum':['open','solved']}),query('sort',{'type':'string','enum':['newest','active'],'default':'newest'})],description='置顶主题恒在最前，随后按newest（发布时间）或active（最后活动时间）倒序。游标绑定排序与全部筛选条件，并携带置顶标志。隐藏、删除内容不出现。board_id或tag_id不存在时返回404而不是空列表；resolution_status只匹配问答，讨论帖没有解决状态。',implemented=True)
operation('post','/topics','createTopic','发布帖子或问题','PublicContent','CreateTopicRequest',status='201',implemented=True)
operation('get','/contents/{content_id}','getContent','读取内容或删除占位','PublicContent',auth='public',description='检查全部祖先可见性；隐藏内容统一404。已删除内容返回不含原文、作者与图片的占位，且仅当其祖先链完整可达时返回——级联删除后的子内容仍按404处理，删除占位不会确认一条已经整体消失的内容。',implemented=True)
operation('get','/contents/{content_id}/permissions','getContentPermissions','读取当前会话内容权限','ContentPermissions',description='私有响应，不做公共缓存；未登录访问返回六项全false，而不是401。最终写入仍需重新鉴权。can_edit为false还可能是超过30分钟编辑时限或处于禁言；can_set_resolution只有提问者为true，管理员不是例外；can_delete当前仅作者本人为true（管理员删除他人内容尚未实现，见开发说明）。',implemented=True)
operation('patch','/contents/{content_id}','editContent','编辑自己的内容','PublicContent','EditContentRequest',implemented=True)
operation('delete','/contents/{content_id}','deleteContent','连带软删除内容','DeletionResult',description='作者删除自己内容或管理员删除。问题删除全部回答与评论；帖子/回答删除下属评论；评论沿真实回复树删除后代。原文30天审核保留，重复删除不延长期限。',implemented=True)
operation('patch','/questions/{topic_id}/resolution','setResolution','切换问题解决状态','QuestionTopic','SetResolutionRequest',description='仅提问者。solved禁止所有新增回答、评论和回复，与新增和删除使用同一根主题锁。',implemented=True)
operation('get','/questions/{topic_id}/answers','listAnswers','读取问题回答','ContentPage',auth='public',params=pagination,description='按发布时间正序返回。讨论帖返回409 NOT_A_QUESTION；根问题不可见或不存在返回404。逐行仍走统一祖先可见性判定，而不是只信任SQL筛选。',implemented=True)
operation('post','/answers','createAnswer','发布回答','Answer','CreateAnswerRequest',status='201',description='根问题必须open。匿名提问者自答强制匿名并显示楼主。',implemented=True)
operation('get','/topics/{topic_id}/comments','listComments','读取主题或回答评论','ContentPage',auth='public',params=pagination+[query('answer_id',{'type':'string'},'省略时读取主题评论'),query('parent_comment_id',{'type':'string'},'省略读取顶层；填写读取该串回复')],description='按发布时间正序返回两层显示所需的两层数据：省略parent_comment_id时只返回顶层评论（整个主题，或answer_id指定的回答下的顶层评论），填写parent_comment_id时返回该串根下的全部回复。目标不可见、不属于该主题或不是串根时返回422 INVALID_COMMENT_TARGET；根主题不可见返回404。逐行仍走统一祖先可见性判定。',implemented=True)
operation('post','/comments','createComment','发布评论或回复','Comment','CreateCommentRequest',status='201',description='匿名楼主强制匿名并显示楼主；解决、锁定、隐藏、删除状态均须检查根主题及目标祖先。',implemented=True)
operation('get','/search','searchContents','搜索公开内容','ContentPage',auth='public',params=pagination+[query('q',{'type':'string','minLength':1}),query('board_id',{'type':'string'}),query('tag_id',{'type':'string'})],description='只检索可见原文，不能检索审核归档；q必填且不超过100字符，按内容id倒序（最新在前，没有相关度信号可排）。匹配是正文子串加标题子串，不是全文索引，中文检索策略仍需语料验证；board_id与tag_id限定"内容所属的根主题"，因此回答与评论也随其主题被筛入或筛出。隐藏、删除内容及其后代不会出现；匿名作者按与其它读取相同的匿名规则渲染。',implemented=True)
spec['paths']['/api/v1/search']['get']['parameters'][-3]['required']=True
op=operation('post','/attachments','uploadImage','上传图片','Attachment',status='201',description='校验真实格式、大小和像素数。接受的图片一律解码后重新编码再落盘，容器里的元数据（EXIF/GPS、XMP、PNG文本块、ICC）不会保存；EXIF方向先应用到像素，剥离后不会转向。重新编码后的字节数超过上限1.25倍时按413拒绝。图片上限在实现阶段定稿。',implemented=True)
op['requestBody']={'required':True,'content':{'multipart/form-data':{'schema':{'type':'object','additionalProperties':False,'required':['file'],'properties':{'file':{'type':'string','format':'binary'}}}}}}
op=operation('get','/media/{key}','getImage','读取受控图片',auth='public',description='检查可见关联或上传者权限，匿名地址不含身份；不直接暴露磁盘目录。',implemented=True)
op['responses']['200']['content']={'image/*':{'schema':{'type':'string','format':'binary'}}}
operation('get','/me/notification-preferences','getNotificationPreferences','读取邮件偏好','NotificationPreferences',description='仅本人可读。未设置过时三项均为 false：邮件是选择性开启的，不替用户发明默认值，也不会向未确认的用户发信；站内通知不受这三项影响。',implemented=True)
operation('put','/me/notification-preferences','setNotificationPreferences','设置邮件偏好','NotificationPreferences','NotificationPreferences',description='整体替换三项：缺少任一项按 422 拒绝，避免"只改一项"被静默当成"其余关掉"。未设置过的账号读取为全 false。',implemented=True)
operation('get','/me/subscriptions/{topic_id}','getSubscription','读取单篇邮件设置','TopicSubscription',description='仅本人可读。未设置过读作 inherit（跟随全局邮件偏好）。主题已删除或不可见时返回404：读者无法为打不开的讨论改邮件设置，而那些邮件本来也不会发出（投递前会重新检查可见性）。',implemented=True)
operation('put','/me/subscriptions/{topic_id}','setSubscription','设置单篇邮件订阅','TopicSubscription','TopicSubscription',description='请求topic_id必须与路径相同，覆盖全局默认。只影响该主题的邮件，不影响站内通知；主题已删除或不可见时返回404。',implemented=True)
operation('post','/subscriptions/unsubscribe','unsubscribeEmail','凭邮件令牌取消订阅',request='UnsubscribeRequest',auth='public',status='204',description='用户打开站内退订确认页后提交，避免邮件扫描器GET误退订；不要求登录，仅校验范围受限的签名令牌和Origin。令牌只命名一个账号与一个主题，因此最坏情况只能关掉它被签发时对应的那个主题的邮件；不能读取任何内容，也不改动全局邮件偏好。签名用独立子密钥，过期时间在签名覆盖的载荷内。',implemented=True)
operation('get','/me/notifications','listNotifications','读取站内通知','NotificationPage',params=pagination,description='仅接收者本人可读。只返回安全摘要与内容定位：不返回作者内部ID、不返回原文快照。actor按内容当前可见性与匿名规则在读取时附加，内容已删除或不可见时省略actor且摘要只说该内容不可见；站内通知是接收者自己的历史，不随删除消失。游标按id倒序翻页，不携带内部用户ID。',implemented=True)
operation('put','/me/notifications/{notification_id}/read','readNotification','标记通知已读',status='204',description='仅接收者本人可操作；重复标记按幂等处理返回204，他人的或不存在的通知一律404，不用于确认他人通知是否存在。',implemented=True)
operation('post','/reports','createReport','举报内容','MyReport','CreateReportRequest',status='201',description='登录用户操作；同一用户对同一内容最多一个未结案举报（表上的唯一键保证，不是先查后写），结案后可再次举报。举报不改变被举报内容：它是主张而不是裁决，因此刷举报本身无法下架帖子；也不向内容作者公开举报者身份。目标不可见时返回404而不是403，避免用举报探测隐藏ID是否存在。',implemented=True)
operation('get','/me/reports','listMyReports','读取本人举报','MyReportPage',params=pagination,description='仅举报者本人可读，按id倒序、游标绑定本列表。resolution_note在结案后出现，未结案时省略（空note不等于已处理）。',implemented=True)
operation('get','/admin/reports','listAdminReports','读取举报队列','AdminReportPage',auth='admin',params=pagination)
operation('patch','/admin/reports/{report_id}','resolveReport','处理举报','AdminReport','AdminResolveReportRequest',auth='admin')
operation('post','/admin/anonymous-traces','traceAnonymous','审计并追溯匿名作者','AdminAnonymousTrace','AdminTraceRequest',auth='admin')
operation('post','/admin/deleted-archives/read','readDeletedArchive','审计并读取删除原文','AdminDeletedArchive','AdminArchiveRequest',auth='admin')
operation('get','/admin/users','listAdminUsers','读取用户管理列表','AdminUserPage',auth='admin',params=pagination)
operation('post','/admin/restrictions','createRestriction','创建禁言或封禁','AdminRestriction','AdminCreateRestrictionRequest',auth='admin',status='201')
operation('get','/admin/restrictions','listRestrictions','读取处罚记录','AdminRestrictionPage',auth='admin',params=pagination)
operation('delete','/admin/restrictions/{restriction_id}','revokeRestriction','撤销处罚',auth='admin',status='204',description='记录撤销时间，不删除处罚记录，写审计。')
operation('get','/admin/audit','listAudit','读取管理审计','AdminAuditPage',auth='admin',params=pagination)
operation('post','/admin/boards','createBoard','创建板块','Board','AdminSaveBoardRequest',auth='admin',status='201')
operation('put','/admin/boards/{board_id}','updateBoard','修改板块','Board','AdminSaveBoardRequest',auth='admin')
operation('post','/admin/tags','createTag','创建标签','Tag','AdminSaveTagRequest',auth='admin',status='201')
operation('put','/admin/tags/{tag_id}','updateTag','修改标签','Tag','AdminSaveTagRequest',auth='admin')
operation('patch','/admin/topics/{topic_id}','moderateTopic','锁定、置顶或移动主题','PublicContent','AdminModerateTopicRequest',auth='admin')
operation('patch','/admin/contents/{content_id}/visibility','setVisibility','隐藏或恢复内容',request='AdminSetVisibilityRequest',auth='admin',status='204')
operation('get','/admin/settings','getAdminSettings','读取站点设置','AdminSettings',auth='admin',description='只有管理员可读取。不返回SMTP密码，只用smtp_password_set说明是否已配置；未配置的SMTP字段省略。',implemented=True)
operation('patch','/admin/settings','updateAdminSettings','修改站点设置','AdminSettings','AdminUpdateSettingsRequest',auth='admin',description='只提交需要改动的字段，至少一项。修改后对后续请求立即生效；写入审计，但不记录密码和字段值。',implemented=True)

output=ROOT/'docs/api/openapi.json'
output.write_text(json.dumps(spec,ensure_ascii=False,indent=2)+'\n',encoding='utf-8',newline='\n')
print(f'Generated {len(schemas)} models and {sum(len(v) for v in spec["paths"].values())} operations: {output}')

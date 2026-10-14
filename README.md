# OpenTalk

面向小型社区的论坛，支持普通讨论、问答与可追溯匿名发言。

技术栈：React / Go / MySQL / Redis，部署到自有服务器。

当前已完成首版数据设计、接口合同和前后端工程基础，并在真实MySQL上通过建表、约束与迁移往返验收。账号闭环已实现：图形验证码、邮箱验证码注册、登录退出、找回密码、本人资料和管理员站点设置，并有真实MySQL+Redis的端到端验收测试。发帖、问答、评论等业务功能尚未实现。

- [第一版需求与验收标准](docs/requirements.md)
- [权限矩阵](docs/permissions.md)
- [数据模型与字段字典](docs/data-model.md)
- [本地接口合同与维护说明](docs/api/README.md)
- [MySQL表结构与事务设计](docs/database-design.md)
- [启动方式与验证说明](docs/development.md)
- [开发路线与决策记录](docs/implementation-plan.md)

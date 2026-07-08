HELP: 子 loop 卡住了，把它转成结构化求助。
任务: {{.Task}}
卡在: {{.BlockedState}}
试过: {{.AttemptsSummary}}
最后错误: {{.LastError}}

只输出 JSON：{"help_request":{"stuck_at":"...","tried":[...],"need_from_human":"..."}}
另：若是验收标准本身错了/不全（criteria-mismatch），need_from_human 写「需更新验收标准：...」，不要自己改标准。

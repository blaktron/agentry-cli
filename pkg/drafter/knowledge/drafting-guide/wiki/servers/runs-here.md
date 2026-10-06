---
title: Runs here
type: list
summary: Declared MCP servers seen to install, start and list their tools in the runner, each with the versions, the runtime, the date and what a QuickAgent must declare for it. Generated from servers.yaml; do not edit by hand.
sources: [doc:servers.yaml]
updated: 2026-10-05
---

# Runs here

These servers were seen to install with the runner's own builder, start with no arguments through the runner's gateway, and list their tools, on the runtime and the date each entry gives. The survey's gateway ran outside a run, with the daemon's default runtime and without a run's Docker filter, so a run can still refuse what it started. That is all an entry says: it is not a judgement of a server's safety or quality, and a later version can behave differently. The list is one thing to look at when you choose a server, not a gate. A server on neither list may be pinned after the checks on [Declared MCP servers](mcp-servers.md), and a server here still needs them: its keys, its hosts, and whether its tools fit the task. An entry speaks for the version it gives; another version of the server is on neither list. [src: doc:servers.yaml]

24 servers.

## npm `@cyanheads/osv-advisory-mcp-server`

- **Versions:** 0.1.15, on Node 24.21.0.
- **Seen:** Started and listed its tools with no key; a call of osv_query_package for flask 0.12 on PyPI returned its advisories through api.osv.dev. Every published version (0.1.0 to 0.1.15) declares engines.node >=24.0.0. Installed by hand on Node 22.23.2 the same day, 0.1.14 and 0.1.15 failed every call (Promise.try is not a function) while 0.1.7 to 0.1.13 answered. [src: doc:servers.yaml]
- **Tools listed:** `osv_get_vulnerability`, `osv_list_ecosystems`, `osv_query_batch`, `osv_query_package`.
- **Needs:** egress: api.osv.dev
- **Needs:** no key
- **Evidence:** survey 2026-10-05; Node 22.23.2 installs by hand the same day.

## npm `@modelcontextprotocol/server-brave-search`

- **Versions:** 0.6.2, on Node 24.21.0.
- **Seen:** Started with a placeholder key and listed its tools; not called. Archived by its authors. [src: doc:servers.yaml]
- **Tools listed:** `brave_local_search`, `brave_web_search`.
- **Needs:** secrets: BRAVE_API_KEY
- **Needs:** egress: api.search.brave.com
- **Evidence:** survey 2026-10-05.

## npm `@modelcontextprotocol/server-everart`

- **Versions:** 0.6.2, on Node 24.21.0.
- **Seen:** Started with a placeholder key and listed its one tool; not called. Archived by its authors. [src: doc:servers.yaml]
- **Tools listed:** `generate_image`.
- **Needs:** secrets: EVERART_API_KEY
- **Needs:** egress: api.everart.ai
- **Evidence:** survey 2026-10-05.

## npm `@modelcontextprotocol/server-everything`

- **Versions:** 2026.8.31, on Node 24.21.0.
- **Seen:** Started and a call of echo answered. It is the MCP project's test server: its tools are demonstrations, not a task's. [src: doc:servers.yaml]
- **Tools listed:** `echo`, `get-annotated-message`, `get-env`, `get-resource-links`, `get-resource-reference`, `get-roots-list`, `get-structured-content`, `get-sum`, `get-tiny-image`, `gzip-file-as-resource`, `simulate-research-query`, `toggle-simulated-logging`, `toggle-subscriber-updates`, `trigger-elicitation-request`, `trigger-long-running-operation`, `trigger-sampling-request`.
- **Needs:** no network, no key
- **Evidence:** survey 2026-10-05.

## npm `@modelcontextprotocol/server-github`

- **Versions:** 2025.4.8, on Node 24.21.0.
- **Seen:** Started with a placeholder key and listed its tools; not called. Its authors have archived it; github-mcp-server-js is also on this list. [src: doc:servers.yaml]
- **Tools listed:** `add_issue_comment`, `create_branch`, `create_issue`, `create_or_update_file`, `create_pull_request`, `create_pull_request_review`, `create_repository`, `fork_repository`, `get_file_contents`, `get_issue`, `get_pull_request`, `get_pull_request_comments`, `get_pull_request_files`, `get_pull_request_reviews`, `get_pull_request_status`, `list_commits`, `list_issues`, `list_pull_requests`, `merge_pull_request`, `push_files`, `search_code`, `search_issues`, `search_repositories`, `search_users`, `update_issue`, `update_pull_request_branch`.
- **Needs:** secrets: GITHUB_PERSONAL_ACCESS_TOKEN
- **Needs:** egress: api.github.com
- **Evidence:** survey 2026-10-05.

## npm `@modelcontextprotocol/server-google-maps`

- **Versions:** 0.6.2, on Node 24.21.0.
- **Seen:** Started with a placeholder key and listed its tools; not called. Archived by its authors. [src: doc:servers.yaml]
- **Tools listed:** `maps_directions`, `maps_distance_matrix`, `maps_elevation`, `maps_geocode`, `maps_place_details`, `maps_reverse_geocode`, `maps_search_places`.
- **Needs:** secrets: GOOGLE_MAPS_API_KEY
- **Needs:** egress: maps.googleapis.com
- **Evidence:** survey 2026-10-05.

## npm `@modelcontextprotocol/server-memory`

- **Versions:** 2026.8.31, on Node 24.21.0.
- **Seen:** Started and a call of read_graph answered. By default it writes its memory file into its own installed files, which are read-only: in two runs the same day, create_entities failed with EROFS, and with MEMORY_FILE_PATH under /tmp it succeeded. The memory lasts only for the run. [src: doc:servers.yaml]
- **Tools listed:** `add_observations`, `create_entities`, `create_relations`, `delete_entities`, `delete_observations`, `delete_relations`, `open_nodes`, `read_graph`, `search_nodes`.
- **Needs:** env: MEMORY_FILE_PATH: /tmp/memory.jsonl (without it, every write fails)
- **Needs:** no network, no key
- **Evidence:** survey 2026-10-05; create_entities runs with and without MEMORY_FILE_PATH the same day.

## npm `@modelcontextprotocol/server-sequential-thinking`

- **Versions:** 2026.8.31, on Node 24.21.0.
- **Seen:** Started and a call of sequentialthinking answered. [src: doc:servers.yaml]
- **Tools listed:** `sequentialthinking`.
- **Needs:** no network, no key
- **Evidence:** survey 2026-10-05.

## npm `@modelcontextprotocol/server-slack`

- **Versions:** 2025.4.25, on Node 24.21.0.
- **Seen:** Started with placeholder keys and listed its tools; not called. Archived by its authors. [src: doc:servers.yaml]
- **Tools listed:** `slack_add_reaction`, `slack_get_channel_history`, `slack_get_thread_replies`, `slack_get_user_profile`, `slack_get_users`, `slack_list_channels`, `slack_post_message`, `slack_reply_to_thread`.
- **Needs:** secrets: SLACK_BOT_TOKEN, SLACK_TEAM_ID
- **Needs:** egress: slack.com
- **Evidence:** survey 2026-10-05.

## npm `@notionhq/notion-mcp-server`

- **Versions:** 2.5.2, on Node 24.21.0.
- **Seen:** Started with a placeholder key and listed its tools; not called. [src: doc:servers.yaml]
- **Tools listed:** `API-create-a-comment`, `API-create-a-data-source`, `API-delete-a-block`, `API-get-block-children`, `API-get-self`, `API-get-user`, `API-get-users`, `API-list-data-source-templates`, `API-move-page`, `API-patch-block-children`, `API-patch-page`, `API-post-page`, `API-post-search`, `API-query-data-source`, `API-retrieve-a-block`, `API-retrieve-a-comment`, `API-retrieve-a-data-source`, `API-retrieve-a-database`, `API-retrieve-a-page`, `API-retrieve-a-page-property`, `API-retrieve-page-markdown`, `API-update-a-block`, `API-update-a-data-source`, `API-update-page-markdown`.
- **Needs:** secrets: NOTION_TOKEN
- **Needs:** egress: api.notion.com
- **Evidence:** survey 2026-10-05.

## npm `@sentry/mcp-server`

- **Versions:** 0.42.0, on Node 24.21.0.
- **Seen:** Started with a placeholder key and listed its tools; not called. [src: doc:servers.yaml]
- **Tools listed:** `analyze_issue_with_seer`, `execute_sentry_tool`, `find_organizations`, `find_projects`, `get_sentry_resource`, `search_events`, `search_issues`, `search_sentry_tools`, `update_issue`.
- **Needs:** secrets: SENTRY_ACCESS_TOKEN
- **Needs:** egress: sentry.io
- **Evidence:** survey 2026-10-05.

## npm `@supabase/mcp-server-supabase`

- **Versions:** 0.13.0, on Node 24.21.0.
- **Seen:** Started with a placeholder key and listed its tools once both hosts were declared; with api.supabase.com alone, tools/list failed (fetch failed), because it also reads supabase.com when it lists its tools. [src: doc:servers.yaml]
- **Tools listed:** `apply_migration`, `confirm_cost`, `create_branch`, `create_project`, `delete_branch`, `deploy_edge_function`, `execute_sql`, `generate_typescript_types`, `get_advisors`, `get_cost`, `get_edge_function`, `get_organization`, `get_project`, `get_project_url`, `get_publishable_keys`, `list_branches`, `list_edge_functions`, `list_extensions`, `list_migrations`, `list_organizations`, `list_projects`, `list_tables`, `merge_branch`, `pause_project`, `query_logs`, `rebase_branch`, `reset_branch`, `restore_project`, `search_docs`.
- **Needs:** secrets: SUPABASE_ACCESS_TOKEN
- **Needs:** egress: api.supabase.com and supabase.com
- **Evidence:** survey 2026-10-05.

## npm `@upstash/context7-mcp`

- **Versions:** 4.1.1, on Node 24.21.0.
- **Seen:** Started with no key; a call of resolve-library-id for React returned matching library IDs. [src: doc:servers.yaml]
- **Tools listed:** `query-docs`, `resolve-library-id`.
- **Needs:** egress: context7.com, mcp.context7.com
- **Needs:** no key for light use; CONTEXT7_API_KEY for more
- **Evidence:** survey 2026-10-05.

## npm `firecrawl-mcp`

- **Versions:** 3.27.3, on Node 24.21.0.
- **Seen:** Started with a placeholder key and listed its tools; not called. [src: doc:servers.yaml]
- **Tools listed:** `firecrawl_agent`, `firecrawl_agent_status`, `firecrawl_check_crawl_status`, `firecrawl_crawl`, `firecrawl_credit_usage`, `firecrawl_developer_search`, `firecrawl_feedback`, `firecrawl_find_tools`, `firecrawl_interact`, `firecrawl_interact_stop`, `firecrawl_map`, `firecrawl_monitor_check`, `firecrawl_monitor_checks`, `firecrawl_monitor_create`, `firecrawl_monitor_delete`, `firecrawl_monitor_get`, `firecrawl_monitor_list`, `firecrawl_monitor_run`, `firecrawl_monitor_update`, `firecrawl_parse`, `firecrawl_research_inspect_paper`, `firecrawl_research_read_paper`, `firecrawl_research_related_papers`, `firecrawl_research_search_papers`, `firecrawl_scrape`, `firecrawl_search`, `firecrawl_search_feedback`.
- **Needs:** secrets: FIRECRAWL_API_KEY
- **Needs:** egress: api.firecrawl.dev
- **Evidence:** survey 2026-10-05.

## npm `frankfurter-mcp-server`

- **Versions:** 1.0.0, on Node 24.21.0.
- **Seen:** Started with no key; a call of frankfurter_get_latest_rates for EUR returned the day's rates from api.frankfurter.dev. [src: doc:servers.yaml]
- **Tools listed:** `frankfurter_convert_currency`, `frankfurter_get_historical_rates`, `frankfurter_get_latest_rates`, `frankfurter_get_time_series`, `frankfurter_list_currencies`.
- **Needs:** egress: api.frankfurter.dev
- **Needs:** no key
- **Evidence:** survey 2026-10-05.

## npm `github-mcp-server-js`

- **Versions:** 0.1.7, on Node 24.21.0.
- **Seen:** Started with a placeholder key and listed 77 tools with GITHUB_PERMISSION set to read-only; not called, since every call needs a real token. [src: doc:servers.yaml]
- **Tools listed:** `check_repo_starred`, `get_app`, `get_authenticated_user`, `get_branch`, `get_code_scanning_alert`, `get_codespace`, `get_commit`, `get_copilot_organization_details`, `get_copilot_seat_details_for_user`, `get_dependabot_alert`, `get_file_contents`, `get_gist`, `get_global_advisory`, `get_issue`, `get_meta`, `get_org`, `get_org_project`, `get_org_project_item`, `get_package_for_authenticated_user`, `get_package_version_for_authenticated_user`, `get_pull_request`, `get_rate_limit`, `get_repository`, `get_repository_advisory`, `get_secret_scanning_alert`, `get_team_by_name`, `get_tree`, `get_user_by_username`, `get_user_hovercard`, `get_workflow`, `get_workflow_run`, `list_branches`, `list_check_runs_for_ref`, `list_code_scanning_alerts`, `list_codespaces`, `list_comments`, `list_commits`, `list_copilot_seats`, `list_dependabot_alerts`, `list_emojis`, `list_gists`, `list_global_advisories`, `list_installation_repos_for_authenticated_user`, `list_installations_for_authenticated_user`, `list_issues`, `list_labels`, `list_labels_on_issue`, `list_notifications`, `list_org_members`, `list_org_project_fields`, `list_org_project_items`, `list_org_projects`, `list_org_repos`, `list_package_versions_for_authenticated_user`, `list_packages_for_authenticated_user`, `list_pull_request_commits`, `list_pull_request_files`, `list_pull_request_reviews`, `list_pull_requests`, `list_repository_advisories`, `list_secret_scanning_alerts`, `list_starred_repos`, `list_tags`, `list_team_members`, `list_teams`, `list_user_followers`, `list_user_following`, `list_workflow_run_artifacts`, `list_workflow_run_jobs`, `list_workflow_runs`, `list_workflows`, `render_markdown`, `search_code`, `search_commits`, `search_issues`, `search_repos`, `search_users`.
- **Needs:** secrets: GITHUB_TOKEN
- **Needs:** egress: api.github.com
- **Needs:** env: GITHUB_PERMISSION: read-only for the read-only tool set
- **Evidence:** survey 2026-10-05.

## npm `hostinger-api-mcp`

- **Versions:** 2.7.3, on Node 24.21.0.
- **Seen:** Started with a placeholder key and listed its tools; not called. The runner starts the bin named after the package; the package's other bins (hostinger-dns-mcp and the rest) cannot be chosen. [src: doc:servers.yaml]
- **Tools listed:** `execute`, `multi-execute`, `search`.
- **Needs:** secrets: API_TOKEN
- **Needs:** egress: developers.hostinger.com
- **Evidence:** survey 2026-10-05.

## npm `n8n-mcp`

- **Versions:** 2.91.0, on Node 24.21.0.
- **Seen:** Started with no key and listed its documentation tools; not called. Its tools that manage workflows need an n8n instance's address and key. [src: doc:servers.yaml]
- **Tools listed:** `get_node`, `get_template`, `search_nodes`, `search_templates`, `tools_documentation`, `validate_node`, `validate_workflow`.
- **Needs:** env and secrets: N8N_API_URL and N8N_API_KEY for the workflow tools
- **Needs:** egress: the n8n instance's host, served on port 443
- **Evidence:** survey 2026-10-05.

## PyPI `awslabs-aws-api-mcp-server`

- **Versions:** 1.5.6, on Python 3.12.14.
- **Seen:** Started with placeholder keys and listed its tools once servicereference.us-east-1.amazonaws.com was declared; without it, it exits at start (it reads that host's service list when it starts). [src: doc:servers.yaml]
- **Tools listed:** `call_aws`, `suggest_aws_commands`.
- **Needs:** secrets: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY; env: AWS_REGION
- **Needs:** egress: servicereference.us-east-1.amazonaws.com and every AWS endpoint its commands call
- **Evidence:** survey 2026-10-05.

## PyPI `awslabs-aws-documentation-mcp-server`

- **Versions:** 1.2.2, on Python 3.12.14.
- **Seen:** Started with no key; a call of search_documentation returned AWS documentation pages. [src: doc:servers.yaml]
- **Tools listed:** `read_documentation`, `read_sections`, `recommend`, `search_documentation`, `search_table`.
- **Needs:** egress: docs.aws.amazon.com, proxy.search.docs.aws.amazon.com
- **Needs:** no key
- **Evidence:** survey 2026-10-05.

## PyPI `ha-mcp`

- **Versions:** 3.5.1, on Python 3.12.14.
- **Seen:** Started with a placeholder token and listed its tools once its URL and token were set; without them it exits at start. 107 newer releases need a newer Python, so 3.5.1 is the newest that admits Python 3.12.14. [src: doc:servers.yaml]
- **Tools listed:** `ha_backup_create`, `ha_backup_restore`, `ha_bulk_control`, `ha_call_service`, `ha_config_delete_dashboard`, `ha_config_get_automation`, `ha_config_get_dashboard`, `ha_config_get_script`, `ha_config_list_dashboards`, `ha_config_list_helpers`, `ha_config_remove_automation`, `ha_config_remove_helper`, `ha_config_remove_script`, `ha_config_set_automation`, `ha_config_set_dashboard`, `ha_config_set_helper`, `ha_config_set_script`, `ha_config_update_dashboard_metadata`, `ha_deep_search`, `ha_eval_template`, `ha_get_bulk_status`, `ha_get_card_documentation`, `ha_get_card_types`, `ha_get_dashboard_guide`, `ha_get_domain_docs`, `ha_get_logbook`, `ha_get_operation_status`, `ha_get_overview`, `ha_get_state`, `ha_search_entities`.
- **Needs:** env: HOMEASSISTANT_URL; secrets: HOMEASSISTANT_TOKEN
- **Needs:** egress: the Home Assistant host, which must be served over HTTPS on port 443 at a name the run can reach (Home Assistant's own port, 8123, cannot be reached)
- **Evidence:** survey 2026-10-05.

## PyPI `mcp-atlassian`

- **Versions:** 0.23.1, on Python 3.12.14.
- **Seen:** Started with a placeholder key and listed its tools; not called. [src: doc:servers.yaml]
- **Tools listed:** `jira_add_comment`, `jira_add_issues_to_sprint`, `jira_add_watcher`, `jira_add_worklog`, `jira_assign_issue`, `jira_batch_create_issues`, `jira_batch_create_versions`, `jira_batch_get_changelogs`, `jira_create_customer_request`, `jira_create_issue`, `jira_create_issue_link`, `jira_create_remote_issue_link`, `jira_create_sprint`, `jira_create_version`, `jira_delete_issue`, `jira_download_attachments`, `jira_edit_comment`, `jira_get_agile_boards`, `jira_get_all_projects`, `jira_get_board_issues`, `jira_get_create_fields`, `jira_get_cross_project_dependencies`, `jira_get_field_options`, `jira_get_issue`, `jira_get_issue_dates`, `jira_get_issue_development_info`, `jira_get_issue_images`, `jira_get_issue_proforma_forms`, `jira_get_issue_sla`, `jira_get_issue_watchers`, `jira_get_issues_development_info`, `jira_get_link_types`, `jira_get_proforma_form_details`, `jira_get_project_components`, `jira_get_project_epic_hierarchy`, `jira_get_project_fields`, `jira_get_project_issue_types`, `jira_get_project_issues`, `jira_get_project_versions`, `jira_get_queue_issues`, `jira_get_request_type_fields`, `jira_get_request_types`, `jira_get_service_desk_for_project`, `jira_get_service_desk_queues`, `jira_get_sprint_issues`, `jira_get_sprints_from_board`, `jira_get_transitions`, `jira_get_user_profile`, `jira_get_worklog`, `jira_link_to_epic`, `jira_move_issue`, `jira_move_issues_to_backlog`, `jira_remove_issue_link`, `jira_remove_watcher`, `jira_search`, `jira_search_assignable_users`, `jira_search_fields`, `jira_search_projects`, `jira_transition_issue`, `jira_update_issue`, `jira_update_proforma_form_answers`, `jira_update_sprint`, `jira_update_version`.
- **Needs:** env: JIRA_URL, JIRA_USERNAME (and the Confluence equivalents)
- **Needs:** secrets: JIRA_API_TOKEN (and CONFLUENCE_API_TOKEN)
- **Needs:** egress: the site's host, such as <site>.atlassian.net
- **Evidence:** survey 2026-10-05.

## PyPI `mcp-server-fetch`

- **Versions:** 2026.8.18, on Python 3.12.14.
- **Seen:** Started and a call of fetch for https://example.com/ returned the page as Markdown. It reads the site's robots.txt from the same host first. [src: doc:servers.yaml]
- **Tools listed:** `fetch`.
- **Needs:** egress: every host it will fetch from, each on port 443
- **Needs:** no key
- **Evidence:** survey 2026-10-05.

## PyPI `mcp-server-time`

- **Versions:** 2026.8.18, on Python 3.12.14.
- **Seen:** Started and a call of get_current_time for UTC answered. [src: doc:servers.yaml]
- **Tools listed:** `convert_time`, `get_current_time`.
- **Needs:** no network, no key
- **Evidence:** survey 2026-10-05.

# Runner tools

Generated from the registry's own table of the runner's tools by the Agentry CLI's tests; do not edit by hand.

## The tools a contract line grants

An instruction that calls for one of these needs its line in the manifest; without it a run does not have the tool, and the registry reports the line as missing.

- `capabilities.browser`: `browser_click`, `browser_close`, `browser_console_messages`, `browser_drag`, `browser_drop`, `browser_emulate_media`, `browser_evaluate`, `browser_fill_form`, `browser_find`, `browser_generate_locator`, `browser_handle_dialog`, `browser_hover`, `browser_navigate`, `browser_navigate_back`, `browser_network_request`, `browser_network_requests`, `browser_pdf_save`, `browser_press_key`, `browser_resize`, `browser_select_option`, `browser_snapshot`, `browser_tabs`, `browser_take_screenshot`, `browser_type`, `browser_verify_element_visible`, `browser_verify_list_visible`, `browser_verify_text_visible`, `browser_verify_value`, `browser_wait_for`.
- `capabilities.exec.commands`: `run_command`.
- `capabilities.network.egress`: `fetch`.
- `capabilities.targetHosts`: `host_get`, `host_put`, `host_read`, `host_run`, `host_shell_close`, `host_shell_open`, `host_shell_read`, `host_shell_send`, `host_write`.
- `capabilities.filesystem.read`: `fs_list`, `fs_read`.
- `capabilities.filesystem.write`: `fs_write`.
- `capabilities.mcpServers`: a declared server's own tools, named `<server>__<tool>`.
- `capabilities.browser.secrets`: a browser that may type secrets has every browser tool but `browser_evaluate`.

## The tools no line grants

The runner gives these without a contract line: the inputs, the report, the scratch folder and its search, and `export_pdf` to every run; the knowledge and voice tools to a run a person attaches a pack or a voice to; the rest to the application's own agents. An instruction that calls for one is never a contract breach.

- `ai_review`, `end_blocked`, `export_pdf`, `glob`, `grep`, `knowledge_list`, `knowledge_read`, `knowledge_search`, `list_inputs`, `read_input`, `registry_search`, `scratch_list`, `scratch_read`, `scratch_search`, `scratch_write`, `search_inputs`, `voice_read`, `web_read`, `web_search`, `write_report`.

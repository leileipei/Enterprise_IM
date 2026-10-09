import 'dart:async';
import 'package:flutter/material.dart';
import '../features/directory/directory_controller.dart';
import '../features/memberships/membership_controller.dart';
import 'workspace_action_page.dart';
import '../features/groups/group_controller.dart';

enum DirectoryMode { browse, create, pick }

class DirectoryPage extends StatefulWidget {
  final MembershipController memberships;
  final DirectoryMode mode;
  const DirectoryPage({
    super.key,
    required this.memberships,
    this.mode = DirectoryMode.browse,
  });
  @override
  State<DirectoryPage> createState() => _DirectoryPageState();
}

class _DirectoryPageState extends WorkspaceActionState<DirectoryPage> {
  @override
  MembershipController get memberships => widget.memberships;
  late final DirectoryController _directory;
  final _query = TextEditingController(), _name = TextEditingController();
  PeoplePage? _page;
  final _selected = <String, DirectoryRole>{};
  bool _searched = false;
  final _selectedPeople = <String, String>{};
  final _selectedNames = <String, String>{};
  @override
  bool get pendingMatches =>
      pending?.kind == ActionKind.create || pending?.kind == ActionKind.direct;
  @override
  void initState() {
    super.initState();
    _directory = DirectoryController(memberships);
    unawaited(run(() async {}));
  }

  @override
  void withdraw() {
    _page = null;
    _selected.clear();
    _selectedPeople.clear();
    _selectedNames.clear();
    if (expired) {
      _directory.close();
      _query.clear();
      _name.clear();
    }
  }

  Future<void> _search() => run(() async {
    // Withdraw results before each query, including malformed queries.
    _page = null;
    _searched = true;
    final result = await _directory.search(_query.text);
    if (active) _page = result;
  });
  @override
  void dispose() {
    _directory.close();
    _query.dispose();
    _name.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final create = widget.mode == DirectoryMode.create;
    final enabled = active && !busy && pending == null;
    return Scaffold(
      appBar: pageBar(
        create
            ? '创建群聊'
            : widget.mode == DirectoryMode.pick
            ? '选择人员任职'
            : '集团通讯录',
      ),
      body: SafeArea(
        child: ListView(
          padding: const EdgeInsets.all(16),
          children: [
            operationStatus,
            if (!expired) ...[
              TextField(
                key: const ValueKey('directory-search'),
                controller: _query,
                enabled: !busy,
                textInputAction: TextInputAction.search,
                onSubmitted: (_) => _search(),
                maxLength: 100,
                decoration: const InputDecoration(labelText: '姓名或工号（至少2个字符）'),
              ),
              Align(
                alignment: Alignment.centerLeft,
                child: FilledButton(
                  onPressed: busy ? null : _search,
                  child: const Text('搜索'),
                ),
              ),
              if (create)
                TextField(
                  key: const ValueKey('group-name'),
                  controller: _name,
                  enabled: enabled,
                  maxLength: 120,
                  decoration: const InputDecoration(labelText: '群名称'),
                ),
              for (final person in _page?.people ?? <DirectoryPerson>[])
                Card(
                  child: Padding(
                    padding: const EdgeInsets.all(12),
                    child: Column(
                      crossAxisAlignment: CrossAxisAlignment.stretch,
                      children: [
                        Text(
                          person.name,
                          style: Theme.of(context).textTheme.titleMedium,
                        ),
                        if (person.employeeNo.isNotEmpty)
                          Text('工号：${person.employeeNo}'),
                        for (final role in person.memberships)
                          if (person.id != admission.client.session.userId) ...[
                            if (create)
                              CheckboxListTile(
                                key: ValueKey('directory-role-${role.id}'),
                                value: _selected.containsKey(role.id),
                                title: Text(role.organizationName),
                                subtitle: Text(
                                  [
                                    role.title,
                                    ...role.departments,
                                  ].where((v) => v.isNotEmpty).join(' · '),
                                ),
                                onChanged: !enabled
                                    ? null
                                    : (value) => setState(() {
                                        if (value == true) {
                                          final previous =
                                              _selectedPeople[person.id];
                                          if (previous != null) {
                                            _selected.remove(previous);
                                            _selectedNames.remove(previous);
                                          }
                                          if (_selected.length < 20) {
                                            _selected[role.id] = role;
                                            _selectedPeople[person.id] =
                                                role.id;
                                            _selectedNames[role.id] =
                                                person.name;
                                          }
                                        } else {
                                          _selected.remove(role.id);
                                          _selectedNames.remove(role.id);
                                          _selectedPeople.remove(person.id);
                                        }
                                      }),
                              ),
                            if (!create)
                              ListTile(
                                title: Text(role.organizationName),
                                subtitle: Text(
                                  [
                                    role.title,
                                    ...role.departments,
                                  ].where((v) => v.isNotEmpty).join(' · '),
                                ),
                                trailing: TextButton(
                                  onPressed: !enabled
                                      ? null
                                      : () => widget.mode == DirectoryMode.pick
                                            ? Navigator.pop(context, role)
                                            : execute(
                                                () => actions.startDirect(
                                                  role.id,
                                                ),
                                              ),
                                  child: Text(
                                    widget.mode == DirectoryMode.pick
                                        ? '选择此任职'
                                        : '发起单聊',
                                  ),
                                ),
                              ),
                          ],
                      ],
                    ),
                  ),
                ),
              if (_searched && _page != null && _page!.people.isEmpty)
                const Text('没有符合条件的可见人员'),
              if (_page?.hasMore == true) const Text('还有更多可见人员，请缩小搜索范围。'),
              if (create) ...[
                Text('已选 ${_selected.length} 人（最多20人，请每人选择一个任职）'),
                for (final role in _selected.values)
                  Text(role.organizationName),
                FilledButton(
                  onPressed: !enabled || _selected.isEmpty
                      ? null
                      : () => execute(
                          () => actions.create(
                            _name.text,
                            _selected.keys.toList(),
                          ),
                        ),
                  child: const Text('确认创建群聊'),
                ),
              ],
            ],
          ],
        ),
      ),
    );
  }
}

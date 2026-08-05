# 生产使用说明

此目录中的 Python/PyQt 地图工具仅保留用于历史行为对照，不属于 L4D2 Panel 的受支持生产链路，也不会参与 Control、Agent 或便携发布产物的构建。

生产环境的地图导入、归档解压、VPK 校验/合并/清理、字典检测、对象存储和实例分配均由 `internal/agent` 中的纯 Go 无界面核心完成。节点不需要 Python、PyQt、7-Zip、UnRAR 或 `left4dead2.exe`。

Python 工具曾提供的字典重建功能不会迁移到 Agent。生产核心发现 BSP 缺少 `stringtable_dictionary.dct` 时只记录持久警告，不修改 BSP，也不阻止导入或分配。

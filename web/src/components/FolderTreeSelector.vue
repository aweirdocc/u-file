<script setup lang="ts">
// 可视化目录树权限选择器：
// - 默认全选（modelValue = [] 表示整源全部目录可访问）
// - 支持按需展开文件源的真实目录结构（懒加载子文件夹）
// - 支持父子联动勾选、在全选状态下取消任意目录自动分裂保留其余兄弟目录、全勾回时自动向上收敛
import { computed, onMounted, ref, watch } from 'vue'
import { Folder, FolderOpened, ArrowRight, CaretBottom, CaretRight, Refresh } from '@element-plus/icons-vue'
import { listFiles } from '../api'

const props = defineProps<{
  sourceId: string
  /** 允许访问的路径前缀列表；空数组 [] 代表「全选（整源全部目录）」 */
  modelValue: string[]
}>()

const emit = defineEmits<{
  'update:modelValue': [prefixes: string[]]
}>()

export interface FolderNode {
  path: string
  name: string
  loaded: boolean
  loading: boolean
  expanded: boolean
  children: FolderNode[]
}

const treeOpen = ref(props.modelValue.length > 0)
const rootLoading = ref(false)
const rootLoaded = ref(false)
const rootError = ref('')
const rootChildren = ref<FolderNode[]>([])
const customPathInput = ref('')

/** 空数组表示全选整源目录（且未处于“全部取消”的临时状态） */
const noneSelected = ref(false)

const isAllSelected = computed(() => !noneSelected.value && props.modelValue.length === 0)

/** 根复选框的半选状态：非全选且选了至少一个子目录 */
const isRootIndeterminate = computed(() => !isAllSelected.value && props.modelValue.length > 0)

watch(
  () => props.modelValue,
  (val) => {
    if (val.length > 0) {
      noneSelected.value = false
    }
  },
)

onMounted(() => {
  if (treeOpen.value) {
    ensureRootLoaded()
  }
})

async function fetchSubDirs(dirPath: string): Promise<FolderNode[]> {
  const res = await listFiles(props.sourceId, dirPath, { limit: 500 })
  const dirs = (res.entries || []).filter(e => e.is_dir)
  return dirs.map(d => ({
    path: d.path.replace(/^\/+|\/+$/g, ''),
    name: d.name,
    loaded: false,
    loading: false,
    expanded: false,
    children: [],
  }))
}

async function ensureRootLoaded(force = false) {
  if (rootLoaded.value && !force) return
  rootLoading.value = true
  rootError.value = ''
  try {
    rootChildren.value = await fetchSubDirs('')
    rootLoaded.value = true
    // 若已有选中的深层子目录前缀，自动沿路径展开对应的父目录
    for (const pfx of props.modelValue) {
      if (pfx.includes('/')) {
        await expandAlongPath(pfx)
      }
    }
  } catch (e: any) {
    rootError.value = e.message || '加载目录失败'
  } finally {
    rootLoading.value = false
  }
}

async function expandAlongPath(targetPrefix: string) {
  const segs = targetPrefix.split('/').filter(Boolean)
  let currentList = rootChildren.value
  let acc = ''
  for (let i = 0; i < segs.length - 1; i++) {
    acc = acc ? `${acc}/${segs[i]}` : segs[i]
    const found = currentList.find(n => n.path === acc)
    if (!found) break
    found.expanded = true
    if (!found.loaded) {
      try {
        found.loading = true
        found.children = await fetchSubDirs(found.path)
        found.loaded = true
      } catch {
        break
      } finally {
        found.loading = false
      }
    }
    currentList = found.children
  }
}

async function toggleExpand(node: FolderNode) {
  if (node.expanded) { covering: {
    node.expanded = false
    return
  }}
  node.expanded = true
  if (!node.loaded) {
    node.loading = true
    try {
      node.children = await fetchSubDirs(node.path)
      node.loaded = true
    } catch {
      /* 忽略单节点加载失败 */
    } finally {
      node.loading = false
    }
  }
}

function toggleTreePanel() {
  treeOpen.value = !treeOpen.value
  if (treeOpen.value) {
    ensureRootLoaded()
  }
}

// —— 勾选与半选纯函数判定 ——

function isNodeChecked(nodePath: string): boolean {
  if (noneSelected.value) return false
  if (props.modelValue.length === 0) return true // 默认全选
  return props.modelValue.some(p => nodePath === p || nodePath.startsWith(p + '/'))
}

function isNodeIndeterminate(nodePath: string): boolean {
  if (isNodeChecked(nodePath)) return false
  return props.modelValue.some(p => p.startsWith(nodePath + '/'))
}

// —— 勾选变更与父子收敛/分裂算法 ——

function onToggleAll(val: boolean) {
  if (val) {
    noneSelected.value = false
    emit('update:modelValue', [])
  } else {
    noneSelected.value = true
    emit('update:modelValue', [])
  }
}

/**
 * 当从某个已全选的祖先目录（ancestorPath，'' 表示根）中取消排除 targetPath 时，
 * 沿着 ancestorPath -> targetPath 的层级，将沿途每一层的其余兄弟文件夹加入选中列表。
 */
function splitAncestorSelection(ancestorPath: string, targetPath: string): string[] {
  const result: string[] = []
  let currentChildren = ancestorPath === '' ? rootChildren.value : findNode(ancestorPath)?.children || []

  const rel = ancestorPath === '' ? targetPath : targetPath.slice(ancestorPath.length + 1)
  const segs = rel.split('/').filter(Boolean)
  let curBase = ancestorPath

  for (let i = 0; i < segs.length; i++) {
    const stepPath = curBase ? `${curBase}/${segs[i]}` : segs[i]
    for (const sibling of currentChildren) {
      if (sibling.path !== stepPath) {
        result.push(sibling.path)
      }
    }
    const nextNode = currentChildren.find(n => n.path === stepPath)
    if (!nextNode) break
    currentChildren = nextNode.children
    curBase = stepPath
  }
  return result
}

function findNode(targetPath: string, list: FolderNode[] = rootChildren.value): FolderNode | null {
  for (const n of list) {
    if (n.path === targetPath) return n
    if (targetPath.startsWith(n.path + '/')) {
      const found = findNode(targetPath, n.children)
      if (found) return found
    }
  }
  return null
}

/**
 * 向上收敛：若某个父目录的所有已加载子目录均被选中，则合并为父目录；
 * 若根目录的所有一级子目录均被选中，则合并为 []（全选）。
 */
function compactUpwards(prefixes: string[]): string[] {
  let set = new Set(prefixes)

  const checkCollapse = (parentPath: string, children: FolderNode[]) => {
    if (!children.length) return
    for (const c of children) {
      if (c.loaded && c.children.length) {
        checkCollapse(c.path, c.children)
      }
    }
    const allChildrenSelected = children.every(c => set.has(c.path))
    if (allChildrenSelected) {
      for (const c of children) set.delete(c.path)
      if (parentPath === '') {
        set.clear() // 根下全部一级目录均选中 → 恢复全选 []
      } else {
        set.add(parentPath)
      }
    }
  }

  if (rootLoaded.value && rootChildren.value.length > 0) {
    checkCollapse('', rootChildren.value)
  }
  return Array.from(set)
}

function onToggleNode(node: FolderNode, checked: boolean) {
  let next: string[] = []

  if (checked) {
    noneSelected.value = false
    if (isAllSelected.value) return
    // 移除该节点下的任何子路径，加入该节点本身
    next = props.modelValue.filter(p => p !== node.path && !p.startsWith(node.path + '/'))
    next.push(node.path)
    next = compactUpwards(next)
    emit('update:modelValue', next)
    return
  }

  // checked === false：取消勾选 node.path
  if (isAllSelected.value) {
    // 从「全选整源 ('')」分裂，排除 node.path
    const split = splitAncestorSelection('', node.path)
    if (split.length === 0) {
      noneSelected.value = true
      emit('update:modelValue', [])
    } else {
      noneSelected.value = false
      emit('update:modelValue', split)
    }
    return
  }

  // 检查是否由某个已选中的祖先前缀覆盖了当前 node.path
  const coveringAncestor = props.modelValue.find(p => node.path.startsWith(p + '/'))
  if (coveringAncestor) {
    next = props.modelValue.filter(p => p !== coveringAncestor)
    next.push(...splitAncestorSelection(coveringAncestor, node.path))
  } else {
    // 移除 node.path 本身及其所有子孙前缀
    next = props.modelValue.filter(p => p !== node.path && !p.startsWith(node.path + '/'))
  }

  if (next.length === 0) {
    noneSelected.value = true
  } else {
    noneSelected.value = false
  }
  emit('update:modelValue', next)
}

function removePrefixTag(pfx: string) {
  const next = props.modelValue.filter(p => p !== pfx)
  if (next.length === 0) {
    noneSelected.value = true
  }
  emit('update:modelValue', next)
}

function addCustomPath() {
  const clean = customPathInput.value.trim().replace(/^\/+|\/+$/g, '')
  if (!clean) return
  customPathInput.value = ''
  noneSelected.value = false
  if (isAllSelected.value) {
    emit('update:modelValue', [clean])
    return
  }
  if (!props.modelValue.includes(clean)) {
    emit('update:modelValue', [...props.modelValue, clean])
  }
}
</script>

<template>
  <div class="folder-selector">
    <!-- 顶部状态与全选控制栏 -->
    <div class="selector-bar">
      <el-checkbox
        :model-value="isAllSelected"
        :indeterminate="isRootIndeterminate"
        @change="(val: any) => onToggleAll(!!val)"
      >
        <b>全选所有目录（整源访问）</b>
      </el-checkbox>

      <el-tag v-if="isAllSelected" size="small" type="success" effect="plain">
        默认全选 · 可访问整源全部目录
      </el-tag>
      <el-tag v-else-if="noneSelected" size="small" type="warning" effect="plain">
        未勾选目录（请至少勾选一个目录或恢复全选）
      </el-tag>
      <el-tag v-else size="small" type="primary" effect="plain">
        已限定 {{ modelValue.length }} 个可访问目录
      </el-tag>

      <div class="spacer" />

      <el-button size="small" :type="treeOpen ? 'primary' : 'default'" plain @click="toggleTreePanel">
        <el-icon style="margin-right: 4px"><FolderOpened v-if="treeOpen" /><Folder v-else /></el-icon>
        {{ treeOpen ? '收起目录树' : '从文件树选择目录' }}
      </el-button>
    </div>

    <!-- 已选中的特定目录标签栏（仅非全选时展示） -->
    <div v-if="!isAllSelected && modelValue.length > 0" class="selected-tags">
      <span class="tag-label muted small">已选目录：</span>
      <el-tag
        v-for="pfx in modelValue"
        :key="pfx"
        size="small"
        closable
        type="primary"
        effect="light"
        @close="removePrefixTag(pfx)"
      >
        📁 /{{ pfx }}
      </el-tag>
      <el-button size="small" link type="primary" @click="onToggleAll(true)">恢复全选</el-button>
    </div>

    <!-- 展开的目录树面板 -->
    <div v-if="treeOpen" class="tree-box" v-loading="rootLoading">
      <div class="tree-toolbar">
        <span class="muted small">勾选允许该用户访问的目录（支持展开子目录精细勾选）：</span>
        <div class="spacer" />
        <el-button size="small" link :icon="Refresh" @click="ensureRootLoaded(true)">刷新目录树</el-button>
      </div>

      <div v-if="rootError" class="tree-empty error">
        {{ rootError }}
      </div>
      <div v-else-if="rootLoaded && rootChildren.length === 0" class="tree-empty muted">
        该文件源根目录下暂无子文件夹（可在下方手动输入子目录路径，或保持默认全选）
      </div>

      <!-- 递归/多级目录列表 -->
      <div v-else class="tree-nodes">
        <!-- 第 1 级目录 -->
        <div v-for="n1 in rootChildren" :key="n1.path" class="tree-item-group">
          <div class="tree-row">
            <span class="expand-icon" @click="toggleExpand(n1)">
              <el-icon v-if="n1.expanded"><CaretBottom /></el-icon>
              <el-icon v-else><CaretRight /></el-icon>
            </span>
            <el-checkbox
              :model-value="isNodeChecked(n1.path)"
              :indeterminate="isNodeIndeterminate(n1.path)"
              @change="(v: any) => onToggleNode(n1, !!v)"
            >
              <span class="folder-name" @click.prevent="toggleExpand(n1)">📁 {{ n1.name }}</span>
              <span class="folder-path muted">/{{ n1.path }}</span>
            </el-checkbox>
          </div>

          <!-- 第 2 级子目录 -->
          <div v-if="n1.expanded" class="tree-children" v-loading="n1.loading">
            <div v-if="n1.loaded && n1.children.length === 0" class="sub-empty muted small">
              无子文件夹
            </div>
            <div v-for="n2 in n1.children" :key="n2.path" class="tree-item-group">
              <div class="tree-row">
                <span class="expand-icon" @click="toggleExpand(n2)">
                  <el-icon v-if="n2.expanded"><CaretBottom /></el-icon>
                  <el-icon v-else><CaretRight /></el-icon>
                </span>
                <el-checkbox
                  :model-value="isNodeChecked(n2.path)"
                  :indeterminate="isNodeIndeterminate(n2.path)"
                  @change="(v: any) => onToggleNode(n2, !!v)"
                >
                  <span class="folder-name" @click.prevent="toggleExpand(n2)">📁 {{ n2.name }}</span>
                  <span class="folder-path muted">/{{ n2.path }}</span>
                </el-checkbox>
              </div>

              <!-- 第 3 级子目录 -->
              <div v-if="n2.expanded" class="tree-children" v-loading="n2.loading">
                <div v-if="n2.loaded && n2.children.length === 0" class="sub-empty muted small">
                  无子文件夹
                </div>
                <div v-for="n3 in n2.children" :key="n3.path" class="tree-row">
                  <span class="expand-icon placeholder"><ArrowRight /></span>
                  <el-checkbox
                    :model-value="isNodeChecked(n3.path)"
                    :indeterminate="isNodeIndeterminate(n3.path)"
                    @change="(v: any) => onToggleNode(n3, !!v)"
                  >
                    <span class="folder-name">📁 {{ n3.name }}</span>
                    <span class="folder-path muted">/{{ n3.path }}</span>
                  </el-checkbox>
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>

      <!-- 底部辅助：支持手动补充输入尚未创建的子目录路径 -->
      <div class="custom-input-row">
        <el-input
          v-model="customPathInput"
          size="small"
          placeholder="也可手动输入目录路径（如 docs/team1），回车添加"
          clearable
          @keydown.enter.prevent="addCustomPath"
        >
          <template #append>
            <el-button size="small" @click="addCustomPath">添加目录</el-button>
          </template>
        </el-input>
      </div>
    </div>
  </div>
</template>

<style scoped>
.folder-selector {
  width: 100%;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.selector-bar {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}
.spacer { flex: 1; }
.selected-tags {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
  padding: 6px 10px;
  background: var(--el-fill-color-lighter);
  border-radius: 6px;
}
.tag-label { flex-shrink: 0; }
.tree-box {
  border: 1px solid var(--el-border-color-light);
  border-radius: 6px;
  padding: 10px 12px;
  background: var(--el-bg-color);
}
.tree-toolbar {
  display: flex;
  align-items: center;
  margin-bottom: 6px;
  padding-bottom: 6px;
  border-bottom: 1px solid var(--el-border-color-extra-light);
}
.tree-nodes {
  max-height: 240px;
  overflow-y: auto;
  padding: 2px 0;
}
.tree-row {
  display: flex;
  align-items: center;
  min-height: 28px;
  padding: 1px 4px;
  border-radius: 4px;
}
.tree-row:hover {
  background: var(--el-fill-color-light);
}
.expand-icon {
  width: 20px;
  height: 20px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  color: var(--el-text-color-secondary);
  margin-right: 2px;
}
.expand-icon.placeholder {
  opacity: 0.25;
  cursor: default;
}
.folder-name {
  font-size: 13px;
  color: var(--el-text-color-primary);
}
.folder-path {
  font-size: 11px;
  margin-left: 6px;
  font-family: monospace;
}
.tree-children {
  padding-left: 22px;
  border-left: 1px dashed var(--el-border-color-lighter);
  margin-left: 9px;
}
.sub-empty {
  padding: 4px 0 4px 8px;
}
.tree-empty {
  padding: 16px;
  text-align: center;
  font-size: 12px;
}
.tree-empty.error {
  color: var(--el-color-danger);
}
.custom-input-row {
  margin-top: 8px;
  padding-top: 8px;
  border-top: 1px dashed var(--el-border-color-lighter);
}
.muted { color: var(--el-text-color-secondary); }
.small { font-size: 12px; }
</style>

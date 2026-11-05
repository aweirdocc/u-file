<script setup lang="ts">
// M4.1 跨源/同源流式复制与移动目标选择器弹窗（支持目标源切换、目录逐级浏览与快捷新建目录）。
import { ref, computed } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { FolderAdd, Back } from '@element-plus/icons-vue'
import { useBrowseStore } from '../stores/browse'
import { useTaskStore } from '../stores/tasks'
import { listFiles, mkdir } from '../api'
import { parentOf, joinPath } from '../utils/format'
import type { Entry } from '../api/types'

const visible = defineModel<boolean>({ default: false })

const browse = useBrowseStore()
const taskStore = useTaskStore()

const mode = ref<'copy' | 'move'>('copy')
const sourceId = ref('')
const paths = ref<string[]>([])

const targetSourceId = ref('')
const targetDir = ref('')
const dirEntries = ref<Entry[]>([])
const loadingDirs = ref(false)
const submitting = ref(false)

const writableSources = computed(() =>
  browse.sources.filter(s => s.capabilities.write && (!s.access || s.access.actions.includes('write'))),
)

const folders = computed(() => dirEntries.value.filter(e => e.is_dir))

async function open(opts: { mode: 'copy' | 'move'; sourceId: string; paths: string[] }) {
  mode.value = opts.mode
  sourceId.value = opts.sourceId
  paths.value = [...opts.paths]
  if (!browse.sources.length) {
    await browse.loadSources().catch(() => {})
  }
  const defSrc = writableSources.value.find(s => s.id === opts.sourceId) || writableSources.value[0]
  targetSourceId.value = defSrc ? defSrc.id : opts.sourceId
  targetDir.value = targetSourceId.value === opts.sourceId ? browse.path : ''
  visible.value = true
  await loadTargetDir()
}

async function onTargetSourceChange() {
  targetDir.value = ''
  await loadTargetDir()
}

async function loadTargetDir() {
  if (!targetSourceId.value) return
  loadingDirs.value = true
  try {
    const res = await listFiles(targetSourceId.value, targetDir.value, { limit: 200 })
    dirEntries.value = res.entries || []
  } catch (e: any) {
    dirEntries.value = []
    ElMessage.error('加载目标目录失败：' + e.message)
  } finally {
    loadingDirs.value = false
  }
}

async function enterFolder(p: string) {
  targetDir.value = p
  await loadTargetDir()
}

async function goUp() {
  if (!targetDir.value) return
  targetDir.value = parentOf(targetDir.value)
  await loadTargetDir()
}

async function onQuickMkdir() {
  let name: string
  try {
    ;({ value: name } = await ElMessageBox.prompt('新文件夹名称：', '在目标位置新建文件夹', {
      confirmButtonText: '创建',
      cancelButtonText: '取消',
      inputPattern: /\S+/,
      inputErrorMessage: '名称不能为空',
    }))
  } catch {
    return
  }
  const newPath = joinPath(targetDir.value, name.trim())
  try {
    await mkdir(targetSourceId.value, newPath)
    targetDir.value = newPath
    await loadTargetDir()
  } catch (e: any) {
    ElMessage.error('新建目录失败：' + e.message)
  }
}

async function onSubmit() {
  if (!targetSourceId.value) {
    ElMessage.warning('请选择目标文件源')
    return
  }
  submitting.value = true
  try {
    await taskStore.submitTransfer({
      type: mode.value,
      source_id: sourceId.value,
      target_source_id: targetSourceId.value,
      paths: paths.value,
      dst_dir: targetDir.value,
    })
    ElMessage.success(mode.value === 'copy' ? '已提交复制任务，可在右下角任务中心查看进度' : '已提交移动任务，可在右下角任务中心查看进度')
    visible.value = false
  } catch (e: any) {
    ElMessage.error('提交任务失败：' + e.message)
  } finally {
    submitting.value = false
  }
}

defineExpose({ open })
</script>

<template>
  <el-dialog
    v-model="visible"
    :title="mode === 'copy' ? `复制到…（共 ${paths.length} 项）` : `移动到…（共 ${paths.length} 项）`"
    width="560px"
  >
    <el-form label-width="92px">
      <el-form-item label="源位置">
        <el-tag size="small" type="info">{{ sourceId }}</el-tag>
        <span class="src-summary">
          {{ paths.length === 1 ? paths[0] : `${paths[0]} 等 ${paths.length} 项` }}
        </span>
      </el-form-item>

      <el-form-item label="目标文件源">
        <el-select v-model="targetSourceId" style="width: 100%" @change="onTargetSourceChange">
          <el-option
            v-for="s in writableSources"
            :key="s.id"
            :label="`${s.name || s.id} (${s.type})`"
            :value="s.id"
          />
        </el-select>
      </el-form-item>

      <el-form-item label="目标目录">
        <div class="dir-picker">
          <div class="dir-bar">
            <el-button size="small" :icon="Back" :disabled="!targetDir" @click="goUp">上级</el-button>
            <code class="cur-dir">/{{ targetDir }}</code>
            <el-button size="small" :icon="FolderAdd" @click="onQuickMkdir">新建子目录</el-button>
          </div>

          <div v-loading="loadingDirs" class="dir-list">
            <div v-if="!folders.length" class="dir-empty">
              当前目录下无子文件夹（将直接{{ mode === 'copy' ? '复制' : '移动' }}到此处）
            </div>
            <div
              v-for="f in folders"
              :key="f.path"
              class="dir-item"
              @click="enterFolder(f.path)"
            >
              <span>📁 {{ f.name }}</span>
              <span class="enter-hint">点击进入 →</span>
            </div>
          </div>
        </div>
      </el-form-item>
    </el-form>

    <template #footer>
      <el-button @click="visible = false">取消</el-button>
      <el-button type="primary" :loading="submitting" @click="onSubmit">
        {{ mode === 'copy' ? '开始复制' : '开始移动' }}
      </el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.src-summary {
  margin-left: 8px;
  font-size: 13px;
  color: var(--el-text-color-regular);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.dir-picker {
  width: 100%;
  border: 1px solid var(--el-border-color);
  border-radius: 6px;
  overflow: hidden;
}
.dir-bar {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 10px;
  background: var(--el-fill-color-light);
  border-bottom: 1px solid var(--el-border-color-lighter);
}
.cur-dir {
  flex: 1;
  font-size: 12.5px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.dir-list {
  max-height: 220px;
  overflow-y: auto;
  padding: 4px 0;
}
.dir-empty {
  text-align: center;
  color: var(--el-text-color-secondary);
  font-size: 12.5px;
  padding: 32px 12px;
}
.dir-item {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 7px 14px;
  cursor: pointer;
  font-size: 13px;
}
.dir-item:hover {
  background: var(--el-fill-color-light);
  color: var(--el-color-primary);
}
.enter-hint {
  font-size: 11.5px;
  color: var(--el-text-color-secondary);
}
</style>

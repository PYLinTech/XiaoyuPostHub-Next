<script setup lang="ts">
import FilePreviewDialog from "./FilePreviewDialog.vue";
import { previewRequest, closeFilePreview } from "@/stores/preview";
</script>

<template>
  <FilePreviewDialog :open="!!previewRequest" :file-name="previewRequest?.fileName || ''"
    :source="previewRequest?.source || null" :download-source="previewRequest?.downloadSource || null"
    :preview-allowed="previewRequest?.previewAllowed ?? true"
    :download-allowed="previewRequest?.downloadAllowed ?? true" @close="closeFilePreview" />
</template>

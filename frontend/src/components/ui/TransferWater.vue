<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from "vue";

const props = defineProps<{ ratio: number | null }>();
const canvas = ref<HTMLCanvasElement | null>(null);
let frame = 0, last = 0, shown = 0;
let width = 0, height = 0;
let observer: ResizeObserver | undefined;
let motion: MediaQueryList | undefined;

function stop() {
  cancelAnimationFrame(frame);
  frame = 0;
  last = 0;
}
function start() {
  if (canvas.value && props.ratio !== null && !frame) frame = requestAnimationFrame(draw);
}
function resize() {
  const rect = canvas.value?.getBoundingClientRect();
  width = rect?.width ?? 0;
  height = rect?.height ?? 0;
  start();
}
function draw(time: number) {
  const el = canvas.value;
  if (!el || props.ratio === null) { stop(); return; }
  const context = el.getContext("2d");
  if (!context || !width || !height) { stop(); return; }
  const ctx = context;
  const dpr = window.devicePixelRatio || 1;
  if (el.width !== Math.round(width * dpr) || el.height !== Math.round(height * dpr)) {
    el.width = Math.round(width * dpr);
    el.height = Math.round(height * dpr);
  }
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, width, height);
  const dt = last ? Math.min((time - last) / 1000, .05) : 0;
  last = time;
  const target = Math.max(0, Math.min(1, props.ratio));
  const reduced = motion?.matches ?? false;
  shown = reduced ? target : shown + (target - shown) * (1 - Math.exp(-dt * 4.5));
  const phaseTime = reduced ? 0 : time;

  // Rotate a continuous sine wave: progress sets x, and the wave varies along y.
  function layer(amplitude: number, phase: number, color: string) {
    ctx.beginPath();
    ctx.moveTo(0, 0);
    const a = amplitude * Math.min(1, shown * 20, (1 - shown) * 20);
    for (let y = 0; y <= Math.ceil(height) + 1; y++) {
      ctx.lineTo(width * shown + a * Math.sin(y * 2 * Math.PI / Math.max(height * 1.6, 76) + phase), y);
    }
    ctx.lineTo(0, height + 1);
    ctx.closePath();
    ctx.fillStyle = color;
    ctx.fill();
  }
  layer(3.2 * (1 + .12 * Math.sin(phaseTime * .00065 + 1.4)), phaseTime * .0026 + 1.6, "rgba(96,165,250,.32)");
  layer(5.8 * (1 + .15 * Math.sin(phaseTime * .00085)), -phaseTime * .0038, "rgba(96,165,250,.58)");
  frame = reduced ? 0 : requestAnimationFrame(draw);
}
function motionChanged() { stop(); start(); }
watch(() => props.ratio, value => {
  if (value === null) { stop(); shown = 0; }
  else start();
}, { flush: "post" });
onMounted(() => {
  motion = window.matchMedia("(prefers-reduced-motion: reduce)");
  motion.addEventListener("change", motionChanged);
  observer = new ResizeObserver(resize);
  if (canvas.value) observer.observe(canvas.value);
  resize();
});
onBeforeUnmount(() => {
  stop();
  observer?.disconnect();
  motion?.removeEventListener("change", motionChanged);
});
</script>

<template><canvas v-show="ratio !== null" ref="canvas" class="transfer-water" aria-hidden="true" /></template>

<style scoped>
.transfer-water { position: absolute; inset: 0; width: 100%; height: 100%; pointer-events: none; }
</style>

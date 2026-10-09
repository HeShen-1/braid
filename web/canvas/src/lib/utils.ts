// shadcn 组件的 className 合并在单点收敛:直接用 `cn`(shadcn-ui/cn,
// clsx + tailwind-merge 的编译版后继),不再经 init 生成的 `cn` 包装。
export { cn } from 'cn'

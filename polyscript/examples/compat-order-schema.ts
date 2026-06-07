type Order = {
  id: string;
  total: number;
  tags?: string[];
};

export function summarize(order: Order): string {
  const tagCount = order.tags?.length ?? 0;
  return `${order.id}:${order.total}:${tagCount}`;
}

const sample: Order = { id: "ord-7", total: 42, tags: ["new", "vip"] };
console.log("ts compat", summarize(sample));

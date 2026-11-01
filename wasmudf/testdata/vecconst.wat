;; vecconst -- two arguments, the first of which MUST arrive with the const flag
;; set and only element 0 written.
;;
;; add(k, x) where k is a literal. The module refuses the call if the flag is
;; clear, which turns "Const is passed as a flag rather than materialized into n
;; identical values" into something the guest observes and a test can assert.
;; Materializing it would be writing nine bytes per record to say the same thing
;; n times -- the cost the engine's own Const representation exists to remove.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))

  (func (export "classad_udf_call_vec")
        (param $n i32) (param $args i32) (param $argc i32)
        (param $res i32) (param $decl i32) (result i32)
    (local $k i64)
    (local $stPtr i32) (local $iPtr i32)
    (local $rSt i32) (local $rI i32)
    (local $i i32) (local $off i32)

    (if (i32.ne (local.get $argc) (i32.const 2))
      (then (return (i32.const 1))))
    ;; argument 0: data form, const flag set.
    (if (i32.ne (i32.load (local.get $args)) (i32.const 0))
      (then (return (i32.const 1))))
    (if (i32.ne (i32.load offset=4 (local.get $args)) (i32.const 1))
      (then (return (i32.const 2))))
    ;; argument 1: data form, NOT const.
    (if (i32.ne (i32.load offset=16 (local.get $args)) (i32.const 0))
      (then (return (i32.const 1))))
    (if (i32.ne (i32.load offset=20 (local.get $args)) (i32.const 0))
      (then (return (i32.const 3))))
    ;; Both must be integers; element 0 is the only one argument 0 has.
    (if (i32.ne (i32.load8_u (i32.load offset=8 (local.get $args))) (i32.const 0))
      (then (return (i32.const 1))))

    (local.set $k (i64.load (i32.load offset=12 (local.get $args))))
    (local.set $stPtr (i32.load offset=24 (local.get $args)))
    (local.set $iPtr (i32.load offset=28 (local.get $args)))
    (local.set $rSt (i32.load offset=8 (local.get $res)))
    (local.set $rI (i32.load offset=12 (local.get $res)))

    (block $end
      (loop $next
        (br_if $end (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $off (i32.shl (local.get $i) (i32.const 3)))
        (i32.store8 (i32.add (local.get $rSt) (local.get $i)) (i32.const 0))
        (i64.store (i32.add (local.get $rI) (local.get $off))
          (i64.add (local.get $k)
                   (i64.load (i32.add (local.get $iPtr) (local.get $off)))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $next)))
    (i32.const 0))

  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (if (i32.ne (local.get $argc) (i32.const 2))
      (then (return (i32.const 1))))
    (i32.store (local.get $res) (i32.const 3))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res)
      (i64.add (i64.load offset=8 (local.get $a))
               (i64.load offset=24 (local.get $a))))
    (i32.const 0))
)

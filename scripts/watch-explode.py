"""机械表 3D 拆解 + 复原动画（Blender 5.2, Cycles CPU）。
用法:
  blender -b -P scripts/watch-explode.py -- --still 120
  blender -b -P scripts/watch-explode.py -- --anim
"""
import bpy
import math
import os
import sys
import bmesh

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = ROOT + "/.tmp/watch"
os.makedirs(OUT, exist_ok=True)

argv = sys.argv[sys.argv.index("--") + 1:] if "--" in sys.argv else []
STILL = int(argv[argv.index("--still") + 1]) if "--still" in argv else 0
ANIM = "--anim" in argv

bpy.ops.wm.read_factory_settings(use_empty=True)
scene = bpy.context.scene
scene.render.engine = "CYCLES"
scene.cycles.device = "CPU"
scene.cycles.samples = 64
scene.render.resolution_x = 352
scene.render.resolution_y = 608
scene.render.fps = 24
scene.render.image_settings.file_format = "PNG"

world = bpy.data.worlds.new("W")
scene.world = world
world.use_nodes = True
bg = world.node_tree.nodes["Background"]
bg.inputs[0].default_value = (0.06, 0.063, 0.072, 1)
bg.inputs[1].default_value = 1.0


def mat(name, color, metallic=0.0, rough=0.4, transmission=0.0, ior=1.45):
    m = bpy.data.materials.new(name)
    m.use_nodes = True
    b = m.node_tree.nodes["Principled BSDF"]
    b.inputs["Base Color"].default_value = (*color, 1)
    b.inputs["Metallic"].default_value = metallic
    b.inputs["Roughness"].default_value = rough
    if transmission > 0:
        b.inputs["Transmission Weight"].default_value = transmission
        b.inputs["IOR"].default_value = ior
    return m


STEEL = mat("steel", (0.62, 0.63, 0.66), metallic=1.0, rough=0.22)
DARKSTEEL = mat("darksteel", (0.18, 0.19, 0.21), metallic=1.0, rough=0.3)
BRASS = mat("brass", (0.78, 0.58, 0.26), metallic=1.0, rough=0.28)
GOLD = mat("gold", (0.92, 0.74, 0.38), metallic=1.0, rough=0.2)
GLASS = mat("glass", (0.80, 0.90, 1.0), metallic=0.0, rough=0.05, transmission=0.90, ior=1.52)
RED = mat("red", (0.55, 0.03, 0.03), metallic=0.2, rough=0.3)

# 太阳纹表盘
DIAL = bpy.data.materials.new("dial")
DIAL.use_nodes = True
nt = DIAL.node_tree
bsdf = nt.nodes["Principled BSDF"]
bsdf.inputs["Metallic"].default_value = 0.0
bsdf.inputs["Roughness"].default_value = 0.32
tc = nt.nodes.new("ShaderNodeTexCoord")
wave = nt.nodes.new("ShaderNodeTexWave")
wave.wave_type = "RINGS"
wave.inputs["Scale"].default_value = 6.0
wave.inputs["Distortion"].default_value = 1.5
wave.inputs["Detail"].default_value = 2.0
ramp = nt.nodes.new("ShaderNodeValToRGB")
ramp.color_ramp.elements[0].color = (0.012, 0.02, 0.05, 1)
ramp.color_ramp.elements[1].color = (0.10, 0.14, 0.30, 1)
nt.links.new(tc.outputs["Object"], wave.inputs["Vector"])
nt.links.new(wave.outputs["Fac"], ramp.inputs["Fac"])
nt.links.new(ramp.outputs["Color"], bsdf.inputs["Base Color"])

parts = []


def track(obj, base_z, off):
    obj.location.z = base_z
    parts.append((obj, base_z, off))


def cyl(name, r, h, z, m, verts=96):
    bpy.ops.mesh.primitive_cylinder_add(radius=r, depth=h, vertices=verts, location=(0, 0, z))
    o = bpy.context.object
    o.name = name
    o.data.materials.append(m)
    return o


def box(name, sx, sy, sz, loc, m):
    bpy.ops.mesh.primitive_cube_add(size=1, location=loc)
    o = bpy.context.object
    o.name = name
    o.scale = (sx, sy, sz)
    o.data.materials.append(m)
    return o


def torus(name, R, r, z, m):
    bpy.ops.mesh.primitive_torus_add(major_radius=R, minor_radius=r, location=(0, 0, z),
                                     major_segments=96, minor_segments=16)
    o = bpy.context.object
    o.name = name
    o.data.materials.append(m)
    return o


def open_cyl(name, r, h, z, m):
    bm = bmesh.new()
    bmesh.ops.create_cone(bm, cap_ends=False, cap_tris=False, segments=96,
                          radius1=r, radius2=r, depth=h)
    me = bpy.data.meshes.new(name)
    bm.to_mesh(me)
    bm.free()
    o = bpy.data.objects.new(name, me)
    o.location = (0, 0, z)
    bpy.context.collection.objects.link(o)
    o.data.materials.append(m)
    return o


# 1. 底盖
track(cyl("back", 2.0, 0.16, -1.35, STEEL), -1.35, -5.0)
track(torus("back_bezel", 1.88, 0.07, -1.45, DARKSTEEL), -1.45, -5.0)

# 2. 机芯
track(cyl("movement_plate", 1.72, 0.42, -0.62, DARKSTEEL), -0.62, -2.6)
for i, (rr, zz, ph) in enumerate([(0.62, -0.48, 0.3), (0.48, -0.42, 1.5), (0.38, -0.38, 3.1)]):
    g = cyl(f"gear{i}", rr, 0.06, zz, BRASS, verts=64)
    g.location.x = math.cos(ph) * 0.55
    g.location.y = math.sin(ph) * 0.55
    track(g, zz, -2.6)
    for t in range(16):
        a = t / 16 * 2 * math.pi
        tooth = box(f"g{i}t{t}", rr * 0.16, 0.05, 0.06,
                    (math.cos(ph) * 0.55 + math.cos(a) * rr,
                     math.sin(ph) * 0.55 + math.sin(a) * rr, zz), BRASS)
        tooth.rotation_euler[2] = a
        track(tooth, zz, -2.6)

track(cyl("barrel", 0.55, 0.11, -0.45, BRASS, verts=64), -0.45, -2.6)
track(torus("balance", 0.5, 0.045, -0.3, GOLD), -0.3, -2.6)
for t in range(4):
    sp = box(f"spoke{t}", 0.04, 1.0, 0.03, (0, 0, -0.3), GOLD)
    sp.rotation_euler[2] = t / 4 * math.pi
    track(sp, -0.3, -2.6)
track(torus("bridge", 1.18, 0.16, -0.26, DARKSTEEL), -0.26, -2.6)

# 3. 表壳 + 表耳 + 表冠
track(open_cyl("case", 1.95, 1.05, 0.0, STEEL), 0.0, 0.0)
track(torus("bezel", 1.95, 0.24, 0.62, STEEL), 0.62, 0.0)
for sx in (-1, 1):
    for sy in (-1, 1):
        track(box(f"lug_{sx}_{sy}", 0.42, 0.55, 0.42, (sx * 1.35, sy * 2.02, 0.0), STEEL), 0.0, 0.0)
crown = cyl("crown", 0.2, 0.38, 0.0, STEEL)
crown.location = (2.06, 0, 0.0)
crown.rotation_euler = (0, math.pi / 2, 0)
track(crown, 0.0, 0.0)

# 4. 表盘
track(cyl("dial", 1.82, 0.06, 0.78, DIAL), 0.78, 3.0)
for t in range(12):
    a = t / 12 * 2 * math.pi
    big = 0.18 if t % 3 == 0 else 0.12
    mark = box(f"mark{t}", 0.05 if t % 3 == 0 else 0.03, big, 0.012,
               (math.cos(a) * 1.6, math.sin(a) * 1.6, 0.83), GOLD)
    mark.rotation_euler[2] = a
    track(mark, 0.83, 3.0)
track(box("brand", 0.34, 0.02, 0.012, (0, -1.15, 0.83), GOLD), 0.83, 3.0)

# 5. 指针
h1 = box("hour", 0.24, 1.15, 0.06, (0, 0.42, 1.02), GOLD)
h1.rotation_euler[2] = math.radians(200)
track(h1, 1.02, 4.6)
h2 = box("minute", 0.17, 1.55, 0.06, (0, 0.62, 1.07), GOLD)
h2.rotation_euler[2] = math.radians(75)
track(h2, 1.07, 4.6)
h3 = box("second", 0.065, 1.75, 0.04, (0, 0.66, 1.11), RED)
h3.rotation_euler[2] = math.radians(330)
track(h3, 1.11, 4.6)
track(cyl("hub", 0.15, 0.09, 1.1, GOLD), 1.1, 4.6)

# 6. 表镜
track(cyl("crystal", 1.9, 0.09, 1.72, GLASS), 1.72, 5.2)
track(torus("crystal_rim", 1.9, 0.06, 1.72, STEEL), 1.72, 5.2)

# 相机
bpy.ops.object.empty_add(location=(0, 0, 1.3))
target = bpy.context.object
bpy.ops.object.camera_add(location=(11.0, -19.0, 12.0))
cam = bpy.context.object
cam.data.lens = 52
scene.camera = cam
con = cam.constraints.new("TRACK_TO")
con.target = target
con.track_axis = "TRACK_NEGATIVE_Z"
con.up_axis = "UP_Y"


def area(loc, energy, size, color=(1, 1, 1)):
    bpy.ops.object.light_add(type="AREA", location=loc)
    o = bpy.context.object
    o.data.energy = energy
    o.data.size = size
    o.data.color = color
    return o


area((6, -7, 10), 3200, 8)
area((-7, -5, 5), 900, 7, (0.75, 0.82, 1.0))
area((0, 7, 6), 1400, 6, (1.0, 0.93, 0.8))
area((0, 0, 15), 2800, 7)

F0, F1, F2 = 1, 96, 240


def ease(x):
    x = max(0.0, min(1.0, x))
    return x * x * (3 - 2 * x)


def factor(f):
    if f <= F0:
        return 0.0
    if f < F1:
        return ease((f - F0) / (F1 - F0))
    if f <= 150:
        return 1.0
    return 1.0 - ease((f - 150) / (F2 - 150))


for f in range(F0, F2 + 1):
    e = factor(f)
    for obj, base_z, off in parts:
        obj.location.z = base_z + e * off
        obj.keyframe_insert(data_path="location", frame=f)
    ang = math.radians(-56 + 18 * (f / F2))
    rad = 22.5 - 1.5 * e
    cam.location = (math.cos(ang) * rad, math.sin(ang) * rad, 7.9 + 1.0 * e)
    cam.keyframe_insert(data_path="location", frame=f)

scene.frame_start = F0
scene.frame_end = F2

if STILL:
    scene.frame_set(STILL)
    scene.render.filepath = OUT + f"/still_{STILL}.png"
    bpy.ops.render.render(write_still=True)
elif ANIM:
    scene.render.filepath = OUT + "/f_"
    bpy.ops.render.render(animation=True)
    print("ANIM_DONE")
